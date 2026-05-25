# pv-snapshotter

> [English Documentation](./README.md)

一个 containerd 代理快照器（proxy snapshotter），将 overlayfs 容器的可写层（`upperdir`/`workdir`）重定向到调用方提供的路径（例如已挂载的 PersistentVolume），使容器在挂载卷之外的写入能够以零数据拷贝的方式落到持久化存储上。

## 目录

- [概述](#概述)
- [工作原理](#工作原理)
- [架构设计](#架构设计)
- [快速开始](#快速开始)
  - [前置条件](#前置条件)
  - [构建](#构建)
  - [containerd 配置](#containerd-配置)
  - [RuntimeClass](#runtimeclass)
  - [Pod 注解说明](#pod-注解说明)
- [DaemonSet 部署](#daemonset-部署)
- [Helm Chart](#helm-chart)
- [命令行参数](#命令行参数)
- [可观测性](#可观测性)
- [运维说明](#运维说明)
- [路线图](#路线图)
- [许可证](#许可证)

---

## 概述

`pv-snapshotter` 封装 containerd 原生 overlayfs 快照器，仅重写一个方法：`Mounts()`。当容器所属 Pod 携带配置的 upperdir 注解时，快照器将 `upperdir=` 和 `workdir=` 这两个 overlay 挂载选项重写为指向调用方提供的路径；当注解不存在时，所有调用都透传给原生 overlayfs。

其效果是：容器的可写层落在调用方控制的存储上（通常是挂载在节点上的 PersistentVolume），而非 containerd 默认的快照目录——不提交镜像、不访问镜像仓库，容器启停时也不拷贝任何数据。

### 功能范围与边界

快照器只做一件事，并刻意不涉足其余职责：

- **它会**在注解存在时，将 overlay 的 `upperdir=`/`workdir=` 选项重写为 `<提供的路径>/upper` 和 `<提供的路径>/work`，否则透传给原生 overlayfs。
- **它不会**调用 Kubernetes API 或任何 CSI 驱动。它只读取调用方已经写入的 Pod 注解中的路径。
- **它不会**置备或挂载存储。当 `Mounts()` 执行时，提供的路径必须已经是就绪的挂载点；若不是，则直接失败而非静默回退。
- **它不会**删除或回收提供路径下的内容。`Remove()` 只清理原生快照目录；后端存储的生命周期归属于创建它的一方。

除挂载选项重写之外的一切——置备卷、计算路径、向 Pod 写入注解、决定何时回收数据——都是调用方（例如你自己提供的 operator 或控制器）的职责。

---

## 工作原理

```
kubelet                      containerd                   pv-snapshotter
  │                              │                              │
  │── 挂接并挂载卷 ───────────►  │  （CSI 挂载卷）              │
  │                              │                              │
  │── CRI CreateContainer ──►    │── Prepare(key) ──────────►  │ 透传给原生 overlay
  │                              │                              │
  │── CRI StartContainer ──►     │── Mounts(key) ───────────►  │ 1. 获取原生 overlay 挂载信息
  │                              │                              │ 2. 通过 containerd 客户端
  │                              │                              │    查找 Pod 注解
  │                              │                              │ 3. 若存在注解：
  │                              │                              │    将 upperdir= / workdir=
  │                              │◄── []mount.Mount ──────────  │    重写为提供的路径
  │                              │                              │
  │                         runc 执行 overlay 挂载
  │                         （upperdir 现在位于提供的路径上）
```

**时序保证：** kubelet 在 CRI `CreateContainer` 之前完成卷的挂接与挂载，因此当 `Mounts()` 被调用时，卷已经挂载到节点上。不存在竞争条件。

---

## 架构设计

### 代理快照器（gRPC 插件）

- 通过 Unix socket 提供 containerd 快照 gRPC API
- 通过 `[proxy_plugins.pv-snapshotter]` 在 containerd 中注册——无需修改 containerd 本身
- 封装原生 overlayfs 快照器，所有方法默认委托给原生实现
- 仅修改 `Mounts()`：当存在 upperdir 注解时，重写 overlay 挂载选项

### 通过 RuntimeClass 按需启用

containerd 按 runtime 选择快照器，而非按 Pod 注解选择。唯一的按 Pod 机制是 `RuntimeClass → runtime → snapshotter`。定义一个使用 `pv-snapshotter` 的 containerd runtime 以及匹配的 RuntimeClass；只有设置了对应 `runtimeClassName` 的 Pod 才会走该快照器。

```
Pod.spec.runtimeClassName: pv
  └─► RuntimeClass handler: pv
        └─► containerd runtime: pv
              └─► snapshotter: pv-snapshotter
```

快照器与 `runtime_type` 正交，因此可以与任意 runtime handler 搭配（纯 `runc`、GPU runtime 等）。现有工作负载和 RuntimeClass 不受影响。

### 调用方提供的 Upperdir 路径

可写层重定向到的路径由调用方通过 Pod 注解提供。快照器在 `Mounts()` 时读取该路径，从不调用 Kubernetes API 或 CSI。

常见来源是挂载在节点上的 CSI 卷。例如 OpenEBS ZFS LocalPV（不使用 globalmount 暂存路径）直接挂载到：

```
/var/lib/kubelet/pods/<podUID>/volumes/kubernetes.io~csi/<pvName>/mount
```

如何计算并写入该路径完全由你决定——参见 [Pod 注解说明](#pod-注解说明)。

### 注解解析

快照器从 containerd sandbox 容器扩展 `io.cri-containerd.sandbox.metadata` 中读取注解。工作负载容器通过 `SandboxID` 查找其父 sandbox。

所有注解键均从 `--annotation-prefix` 派生：

| 注解键 | 用途 |
|--------|------|
| `<prefix>/upperdir-path` | upperdir 根路径的字面量，优先级最高。 |
| `<prefix>/upperdir-path-template` | 渲染为路径的 Go `text/template` 模板。 |
| `<prefix>/var.<VarName>` | 注入模板的自定义变量。 |

内置模板变量：`{{.PodUID}}`、`{{.PodName}}`、`{{.PodNamespace}}`。

**示例（路径由 ZFS LocalPV 卷支撑）：**

```yaml
annotations:
  pv-snapshotter.humble-mun.io/upperdir-path-template: >-
    /var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount
  pv-snapshotter.humble-mun.io/var.PVName: pvc-7cb2f1df-8092-4b89-9f19-d2878aa2d3ec
```

---

## 快速开始

### 前置条件

| 组件 | 版本要求 |
|------|---------|
| Linux 内核 | ≥ 5.11（非 root 用户空间 overlayfs）|
| containerd | v2.x |
| Kubernetes | v1.27+ |
| Go（仅构建需要）| 1.26+ |
| CSI 驱动 | 任意支持块存储挂载的驱动（RBD、ZFS LocalPV、本地 PV 等）|

> **不要使用 CephFS** 作为 upperdir 后端——小文件元数据性能差。请使用格式化为 ext4 或 xfs 的块设备。

### 构建

```bash
# 构建 Docker 镜像（amd64，发布版）
make build

# 自定义架构 / 镜像仓库
make build ARCH=arm64 REPO=my-registry/pv-snapshotter VERSION=v0.1.0

# 调试构建（包含 DWARF 调试符号）
make build DEBUG=true
```

生成的镜像基于 `gcr.io/distroless/static-debian12`（无 shell，攻击面最小）。

### containerd 配置

在 `/etc/containerd/config.toml` 中添加代理插件和 runtime 条目：

```toml
# 注册代理快照器
[proxy_plugins.pv-snapshotter]
  type    = "snapshot"
  address = "/var/run/pv-snapshotter/daemon.sock"

# 使用 pv-snapshotter 的 runtime（此处与 runc 搭配）
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.pv]
  runtime_type = "io.containerd.runc.v2"
  snapshotter  = "pv-snapshotter"
```

`snapshotter` 与 `runtime_type` 设置是正交的：若要将 pv-snapshotter 与其他 runtime handler 搭配，保持 `snapshotter = "pv-snapshotter"`，并将 `runtime_type`/`options` 设为该 handler 的取值即可。

> **不要**修改 `[plugins."io.containerd.grpc.v1.cri".containerd].snapshotter`。pv-snapshotter 专门通过 RuntimeClass 引入。

编辑后重启 containerd。pv-snapshotter 必须在 containerd 启动之前运行——参见 [DaemonSet 部署](#daemonset-部署)了解启动顺序。

### RuntimeClass

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: pv
handler: pv
```

应用：

```bash
kubectl apply -f runtimeclass.yaml
```

Pod 通过以下方式选择加入：

```yaml
spec:
  runtimeClassName: pv
```

### Pod 注解说明

下列注解必须由调用方（你自己提供的 operator、控制器或准入 webhook）在 Pod 创建前计算并写入。快照器只负责消费它们。

```yaml
metadata:
  annotations:
    # 方式 1 — 字面量路径（最高优先级）
    pv-snapshotter.humble-mun.io/upperdir-path: /var/lib/kubelet/pods/abc123/volumes/kubernetes.io~csi/pvc-xyz/mount

    # 方式 2 — Go 模板（在 Mounts() 时渲染）
    pv-snapshotter.humble-mun.io/upperdir-path-template: >-
      /var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount
    pv-snapshotter.humble-mun.io/var.PVName: pvc-7cb2f1df-8092-4b89-9f19-d2878aa2d3ec
```

没有上述任何注解的 Pod 将透明地使用原生 overlayfs。

---

## DaemonSet 部署

DaemonSet 在每个节点上运行 `pv-snapshotter`。其自身的 Pod **不得**设置 `runtimeClassName: pv`，否则快照器将依赖自身才能启动。

必要的 `hostPath` 挂载：

| 节点路径 | 容器内挂载路径 | 用途 |
|---------|--------------|------|
| `/var/run/pv-snapshotter/` | `/var/run/pv-snapshotter/` | gRPC socket |
| `/run/containerd/containerd.sock` | `/run/containerd/containerd.sock` | containerd 客户端 |
| `/var/lib/kubelet` | `/var/lib/kubelet` | 使 CSI 挂载路径可见 |

**启动顺序：** containerd 在启动时连接代理插件，如果插件不可用则**不会**自动重连。确保 pv-snapshotter 在 containerd 之前启动：

- `systemd`：使用 `After=` / `Requires=` 指令
- `DaemonSet 升级`：节点隔离（cordon）→ 驱逐工作负载 Pod（drain）→ 重启 pv-snapshotter 和 containerd → 取消隔离（uncordon）

**故障语义：** 如果 `pv-snapshotter` 不可用，设置了 `runtimeClassName: pv` 的 Pod 将无法启动。它们**不会**静默回退到普通 overlayfs——这是有意为之。

---

## Helm Chart

> 🚧 Helm chart 正在积极开发中，将在近期版本中发布。

Chart 将包含：

- DaemonSet，包含正确的 `hostPath` 挂载和 `tolerations`
- 节点范围操作的 RBAC
- 通过 `initContainer`（或 `configMap` + node-config-operator）进行 containerd 配置修补
- RuntimeClass 创建
- 定向上线的 `nodeSelector` / `affinity`
- 可配置的注解前缀和 socket 路径

在 [Issues](../../issues) 标签页跟踪进度。

---

## 命令行参数

所有参数也可通过环境变量设置（大写，`_` 分隔，以 `DAEMON_` 为前缀）。

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--unix-socket-path` | `/var/run/pv-snapshotter/daemon.sock` | gRPC 监听 socket 路径 |
| `--containerd-socket` | `/run/containerd/containerd.sock` | containerd 客户端 socket |
| `--annotation-prefix` | `pv-snapshotter.humble-mun.io` | Pod 注解的 DNS 子域前缀（RFC 1123，不可使用保留域名）|
| `--overlay-snapshotter.root-path` | `/var/lib/containerd` | 原生 overlay 快照器根目录 |
| `--overlay-snapshotter.upper-dir-label` | `false` | 在快照上标记 `containerd.io/snapshot/overlay.upperdir` 标签 |
| `--overlay-snapshotter.sync-remove` | `false` | 同步删除快照 |
| `--overlay-snapshotter.slow-chown` | `false` | ID 映射挂载的慢速 chown |
| `--overlay-snapshotter.mount-options` | `[]` | 传递给 overlayfs 的额外挂载选项（**永不添加 `volatile`**）|

> ⚠️ **永不**将 `volatile` 添加到 `--overlay-snapshotter.mount-options`。它会在异常关机时导致 `upperdir` 数据丢失，与本项目的持久化语义直接矛盾。

---

## 可观测性

### 验证代理插件注册

```bash
ctr plugins ls | grep pv-snapshotter
# 预期：io.containerd.snapshotter.v1  pv-snapshotter  ok
```

### 通过 ctr 直接测试（绕过 K8s 和 CRI）

```bash
ctr --namespace=k8s.io snapshots --snapshotter=pv-snapshotter ls
ctr --namespace=k8s.io run --snapshotter=pv-snapshotter --rm -t docker.io/library/alpine:latest test sh
```

### 确认重定向后的 upperdir 已生效

```bash
# 在节点上查找容器的 overlay 挂载
findmnt -t overlay
# 确认 upperdir= 指向提供的路径，而非 /var/lib/containerd/snapshots/...
```

### 日志详细级别

```bash
# Pod 生命周期事件（方法调用、resolver 步骤、重定向路由）
--v=4

# 同时输出原始 sandbox 元数据 JSON
--v=5

# 关联特定容器的所有日志
grep 'key="k8s.io/31/4856f54d' /var/log/pv-snapshotter.log
```

### 验证状态持久化

```bash
# 1. 在容器内写入文件
kubectl exec -it <pod> -- sh -c 'echo hello > /root/test.txt'

# 2. 删除 Pod（其可写层通常会随之丢失）
kubectl delete pod <pod>

# 3. 重新创建一个引用相同后端路径的 Pod
kubectl apply -f pod.yaml

# 4. 验证文件依然存在
kubectl exec -it <pod> -- cat /root/test.txt
# hello
```

---

## 运维说明

### 数据生命周期由调用方负责

`Remove()` 只清理快照器根目录下的原生 overlay 快照目录。快照器从不删除调用方提供路径下的内容。如果你需要"停止但不丢失"与"销毁并回收"这两种语义的区分，请在拥有后端存储的组件中实现该区分；快照器不做此决定。

### upper/ 与 work/ 必须位于同一文件系统

快照器在提供的路径下创建 `upper/` 和 `work/`。overlayfs 要求二者位于同一文件系统——请确保提供的路径是单个已挂载的卷。

### 存储扩容

如果后端卷是 CSI PVC，扩容遵循标准 CSI 扩展路径：

```bash
kubectl patch pvc <name> -p '{"spec":{"resources":{"requests":{"storage":"20Gi"}}}}'
```

CSI 驱动在线调整块设备大小；`resize2fs`/`xfs_growfs` 在线执行。无需重启容器。

### 让后端路径不可触达

提供的路径就是原始 overlay 上层目录。如果你将它暴露在容器内（例如将 PVC 挂载到某个专用路径），请阻止工作负载直接向其写入——如有需要，使用 AppArmor / SELinux 强制执行。

### `nerdctl commit`

由该快照器支撑的容器可以像任何其他容器一样被 commit：

```bash
nerdctl --namespace=k8s.io --snapshotter=pv-snapshotter commit <container> <image>
```

### 节点重启恢复

pv-snapshotter 重启后，现有运行中的容器仍持有其 overlay 挂载（runc 持有挂载）。引用相同后端路径重新创建的 Pod 在下次 `Mounts()` 调用时会正确重新绑定。

---

## 路线图

### 生产加固（进行中）

- [ ] `Remove()` 时可配置的清理行为（解除绑定 vs. 回收）
- [ ] 节点重启恢复验证
- [ ] 存储扩容端到端测试
- [ ] 错误恢复：挂载失败、父快照缺失、CSI 尚未就绪
- [ ] GC 协调：overlay metadata.db 清理 vs. 后端存储生命周期

### 未来规划

- [ ] 用于 DaemonSet 部署的 Helm Chart
- [ ] 挂载延迟和解析错误的 Prometheus 指标端点
- [ ] 支持 Ceph RBD globalmount 暂存路径（自动路径检测）
- [ ] 多架构镜像构建（arm64）

---

## 许可证

基于 Apache License, Version 2.0 授权。详见 [LICENSE](./LICENSE) 与 [NOTICE](./NOTICE)。
