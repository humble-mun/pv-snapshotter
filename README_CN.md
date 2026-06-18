# pv-snapshotter

[English](README.md) | 简体中文

> ⚠️ **请勿在生产环境中使用 v0.1.3。** v0.1.3 存在已确认的磁盘压力 Bug：由于
> `Cleanup()` 未正确转发给底层 overlay 快照器，容器删除后遗留的孤立快照目录会无限
> 积累在 `/var/lib/containerd/snapshots/` 下，最终导致节点磁盘压力（Disk Pressure）
> 并进入 NotReady 状态。请升级到 **v0.1.4 或更高版本**，该版本会转发 `Cleanup()`
> gRPC 调用并回收孤立目录。

一个 containerd 代理快照器（proxy snapshotter），将 overlayfs 容器的可写层（`upperdir`/`workdir`）重定向到调用方提供的路径（例如已挂载的 PersistentVolume），使容器在挂载卷之外的写入能够以零数据拷贝的方式落到持久化存储上。

**已生产就绪。** 核心快照器、Helm chart、containerd 配置自动化以及变更准入 Webhook 均已实现并端到端验证。

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
- [变更准入 Webhook](#变更准入-webhook)
  - [Webhook 的职责](#webhook-的职责)
  - [注解模板渲染管线](#注解模板渲染管线)
  - [Webhook 前置条件](#webhook-前置条件)
- [DaemonSet 部署](#daemonset-部署)
- [Helm Chart](#helm-chart)
- [命令行参数](#命令行参数)
- [端到端验证](#端到端验证)
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

生成的镜像基于 `gcr.io/distroless/base-debian13`（仅含 glibc，无 shell，攻击面最小）。CGO 已启用，用于支持 containerd 重启所需的 cgo nsenter 前置逻辑。

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

> **如果你使用 Helm chart 并启用了 `webhook.enabled=true`（默认值）**，Webhook 会自动计算并注入这些注解——你无需手动编写。详见[变更准入 Webhook](#变更准入-webhook)。

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

## 变更准入 Webhook

Webhook 默认启用（`values.yaml` 中 `webhook.enabled=true`）。它提供开箱即用的体验：携带选择加入标签的工作负载会被自动注入状态卷、注解以及 pv-backed RuntimeClass——无需手动编写注解。

### Webhook 的职责

当 Pod 被准入且其标签匹配配置的 `objectSelector`（默认：`pv-snapshotter.humble-mun.io/inject: "true"`）时，Webhook 将：

1. **解析控制器 owner**：沿 owner reference 最多向上遍历 `maxOwnerDepth` 层（默认 2 层：pod → ReplicaSet → Deployment），得到 `OwnerName`。
2. **查找关联 PVC**：通过 `pvcNameTemplate`（默认 `{{.OwnerName}}`）查找 PVC，并等待最多 `boundTimeout`（默认 10 s）使其进入 Bound 状态。若超时未绑定则拒绝 Pod——pv-snapshotter 无法在未绑定的卷上准备 overlay upperdir，提前放行只是将失败延迟到节点层。
3. **获取后端 PV** 并提取 `spec.csi.volumeHandle`（非 CSI PV 时为空）。
4. **构建 JSON Patch**：
   - 在 Pod 上打 `upperdir-path-template` 和 `var.PVName` 注解。
   - 向 `spec.volumes` 追加卷 `pv-snapshotter--state`（由 PVC 支撑）。
   - 仅向“主”容器（`spec.containers[0]`）添加挂载点 `/.platform/state`。
   - 将 `runtimeClassName` 改写为 `<基础名>-pv`（如 `runc-pv`），若 Pod 未指定 `runtimeClassName` 则使用 `defaultRuntimeClass`。

卷名 `pv-snapshotter--state` 使用双横线厂商分隔符，以避免与用户定义的卷名冲突。该注入挂载点只用于触发 kubelet 在创建容器前发布 PVC；业务负载不应读写该路径，sidecar 和 init 容器也不会收到该挂载点。

### 注解模板渲染管线

注解经历三层渲染管线：

| 层级 | 渲染方 | 可用变量 | 用途 |
|------|--------|---------|------|
| 1 | Helm | `values.yaml` | 将 `--webhook-annotation-templates` 渲染为 CLI 参数 |
| 2 | Webhook | `.PVName`、`.VolumeHandle`、`.OwnerName`、`.PodName` | 解析存储侧变量；将注解打到 Pod 上 |
| 3 | pv-snapshotter | `.PodUID`、`.PodName`、`.PodNamespace`、`var.*` | 在 `Mounts()` 时解析 Pod 身份变量 |

默认 `upperdir-path-template` 的值：

```
/var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount
```

- `{{.PVName}}` 由 Webhook（第 2 层）解析，替换为实际 PV 名称。
- `{{.PodUID}}` 穿透第 2 层保持原样，由 pv-snapshotter（第 3 层）解析。

`var.PVName` 注解也会被打上已解析的 PV 名称，使任何在第 3 层引用 `{{.PVName}}` 的自定义模板均可使用。

> **关于 `webhook-annotation-templates` 的配置说明：** `values.yaml` 中的 `annotationTemplates` 字段是说明性的——它记录了默认模板文本和三层渲染管线。Helm chart 将这些值渲染为 daemon 容器上的 `--webhook-annotation-templates=...` CLI 参数，而非写入 `daemon.yaml`。这是有意为之：viper 的 YAML 解析器会将所有 map key 转为小写（`var.PVName` → `var.pvname`），导致注解 key 大小写被破坏。通过命令行参数传递则经由 pflag 的 CSV 解析器处理，可完整保留大小写。

### Webhook 前置条件

- 集群中必须安装 **cert-manager** 以签发 Webhook TLS 证书。
- 必须存在名为 `selfsigned` 的 `ClusterIssuer`（可通过 `webhook.clusterIssuerName` 配置）。
- Webhook 监听端口 9443（可通过 `webhook.bindAddress` 配置）。

---

## DaemonSet 部署

DaemonSet 在每个节点上运行 `pv-snapshotter`。其自身的 Pod **不得**设置 `runtimeClassName: pv`，否则快照器将依赖自身才能启动。

必要的 `hostPath` 挂载：

| 节点路径 | 容器内挂载路径 | 用途 |
|---------|--------------|------|
| `/var/run/pv-snapshotter/` | `/var/run/pv-snapshotter/` | gRPC socket |
| `/run/containerd/` | `/run/containerd/` | containerd 客户端（挂载目录而非 socket 文件，确保 containerd 重启后路径仍然有效）|
| `/var/lib/kubelet` | `/var/lib/kubelet` | 使 CSI 挂载路径可见 |

**启动顺序：** containerd 在启动时连接代理插件，如果插件不可用则**不会**自动重连。确保 pv-snapshotter 在 containerd 之前启动：

- `systemd`：使用 `After=` / `Requires=` 指令
- `DaemonSet 升级`：节点隔离（cordon）→ 驱逐工作负载 Pod（drain）→ 重启 pv-snapshotter 和 containerd → 取消隔离（uncordon）

**故障语义：** 如果 `pv-snapshotter` 不可用，设置了 `runtimeClassName: pv` 的 Pod 将无法启动。它们**不会**静默回退到普通 overlayfs——这是有意为之。

---

## Helm Chart

Helm chart 位于 `charts/pv-snapshotter/`。

```bash
helm upgrade --install pv-snapshotter charts/pv-snapshotter \
  --namespace pv-snapshotter-system --create-namespace \
  --set image=your-registry/pv-snapshotter:vX.Y \
  --set "containerdConfig.runtimeClasses={runc,nvidia}"
```

主要参数：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `image` | — | pv-snapshotter 镜像（必填）|
| `containerdConfig.runtimeClasses` | `[]` | 要扩展的基础 runtime handler 名称（如 `runc`、`nvidia`）|
| `containerdConfig.suffix` | `-pv` | 追加到每个基础名称的后缀（`runc` → `runc-pv`）|
| `unixSocketPath` | `/var/run/pv-snapshotter/daemon.sock` | gRPC socket 路径 |
| `annotationPrefix` | `pv-snapshotter.humble-mun.io` | Pod 注解前缀 |
| `tolerations` | control-plane NoSchedule | 节点容忍配置 |
| `webhook.enabled` | `true` | 启用 Webhook、RBAC、cert-manager 证书和 MutatingWebhookConfiguration |
| `webhook.clusterIssuerName` | `selfsigned` | 用于签发 Webhook TLS 证书的 cert-manager ClusterIssuer |
| `webhook.objectSelector` | `matchLabels: pv-snapshotter.humble-mun.io/inject: "true"` | 仅匹配此选择器的 Pod 才会被变更 |
| `webhook.pvcNameTemplate` | `{{.OwnerName}}` | Go 模板 → 要查找的 PVC 名称 |
| `webhook.maxOwnerDepth` | `2` | owner reference 向上遍历层数 |
| `webhook.defaultRuntimeClass` | `runc` | Pod 未指定 runtimeClassName 时使用的基础 RuntimeClass |
| `webhook.runtimeClassSuffix` | `-pv` | 追加到基础名称以构成 pv-backed RuntimeClass 名 |
| `webhook.boundTimeout` | `10s` | 拒绝 Pod 前等待 PVC 绑定的最长时间 |
| `webhook.stateMountPath` | `/.platform/state` | 注入到“主”容器（`spec.containers[0]`）内的挂载路径 |
| `webhook.annotationTemplates` | ZFS LocalPV 默认值 | 注解键→Go 模板值的映射（说明性；见下方说明）|

Chart 部署内容：
- **ConfigMap**（`daemon.yaml`）：通过 viper 配置 daemon（从 `/etc/humble-mun/daemon.yaml` 加载）。
- **DaemonSet**，包含两个容器：
  - `config`（原生 sidecar，`restartPolicy: Always`）：等待 daemon 的 `/readyz` 端点就绪，幂等地修补 `/etc/containerd/config.toml`，必要时通过 cgo nsenter 前置逻辑重启 containerd，然后阻塞至 Pod 终止。
  - `daemon`：pv-snapshotter gRPC 代理快照器（启用时同时提供 Webhook 服务）。
- 每个 `containerdConfig.runtimeClasses` 条目对应一个 **RuntimeClass**，命名为 `<基础名><后缀>`。
- **ServiceAccount**（`automountServiceAccountToken` 随 `webhook.enabled` 取值）。
- 当 `webhook.enabled=true` 时：**ClusterRole** + **ClusterRoleBinding**（PVC/PV 及工作负载控制器只读权限）、cert-manager **Certificate**、ClusterIP **Service**（端口 9443）以及 **MutatingWebhookConfiguration**。

所有 daemon 参数均可通过 `HM_` 前缀环境变量或 `daemon.yaml` 中的条目覆盖。

> **`webhook.annotationTemplates` 说明：** 该字段记录了默认注解模板值，具有说明性。Helm chart 将其渲染为 daemon 容器上的 `--webhook-annotation-templates` CLI 参数，而非写入 `daemon.yaml`，以保留 map key 的大小写（viper 的 YAML 解析会将所有 key 转为小写）。如需自定义注解模板，在 `values.yaml` 中覆盖 `webhook.annotationTemplates` 即可。

---

## 命令行参数

所有参数也可通过环境变量设置（大写，`_` 分隔，以 `HM_` 为前缀）。

### 快照器参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--unix-socket-path` | `/var/run/pv-snapshotter/daemon.sock` | gRPC 监听 socket 路径 |
| `--containerd-socket` | `/run/containerd/containerd.sock` | containerd 客户端 socket |
| `--annotation-prefix` | `pv-snapshotter.humble-mun.io` | Pod 注解的 DNS 子域前缀（RFC 1123，不可使用保留域名）|
| `--overlay-snapshotter.root-path` | `/var/lib/containerd/io.containerd.snapshotter.v1.pv-snapshotter` | 原生 overlay 快照器根目录 |
| `--overlay-snapshotter.upper-dir-label` | `false` | 在快照上标记 `containerd.io/snapshot/overlay.upperdir` 标签 |
| `--overlay-snapshotter.sync-remove` | `false` | 同步删除快照 |
| `--overlay-snapshotter.slow-chown` | `false` | ID 映射挂载的慢速 chown |
| `--overlay-snapshotter.mount-options` | `[]` | 传递给 overlayfs 的额外挂载选项（**永不添加 `volatile`**）|

> ⚠️ **永不**将 `volatile` 添加到 `--overlay-snapshotter.mount-options`。它会在异常关机时导致 `upperdir` 数据丢失，与本项目的持久化语义直接矛盾。

### Webhook 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--webhook-enabled` | `true` | 启用变更准入 Webhook 端点 |
| `--webhook-pvc-name-template` | `{{.OwnerName}}` | Go 模板 → 要查找的 PVC 名称 |
| `--webhook-pvc-selector-template` | `""` | Go 模板 → 标签选择器；名称模板为空时的回退方案 |
| `--webhook-max-owner-depth` | `2` | owner reference 向上遍历层数（0 = 直接使用 Pod 名称）|
| `--webhook-default-runtime-class` | `runc` | Pod 未指定 runtimeClassName 时使用的基础 RuntimeClass |
| `--webhook-runtime-class-suffix` | `-pv` | 追加到基础名称的后缀 |
| `--webhook-bound-timeout` | `10s` | 拒绝 Pod 前等待 PVC 绑定的最长时间 |
| `--webhook-state-mount-path` | `/.platform/state` | 注入到“主”容器（`spec.containers[0]`）内的挂载路径 |
| `--webhook-annotation-templates` | ZFS LocalPV 默认值 | `key=value` CSV 格式的注解键→Go 模板值映射 |

---

## 端到端验证

`demo.yaml` 包含一个 Deployment 和一个 PVC，用于对完整管线进行端到端验证。在安装了 Helm chart 的集群上执行：

```bash
kubectl apply -f demo.yaml
```

Deployment 的 Pod 携带选择加入标签 `pv-snapshotter.humble-mun.io/inject: "true"`。Webhook 解析 owner 链（pod → ReplicaSet → Deployment `demo`），查找 PVC `demo`，等待其绑定，然后注入状态卷、注解以及 `runtimeClassName: runc-pv`。

**验证步骤：**

```bash
# 1. 确认 PVC 已绑定
kubectl get pvc demo

# 2. 确认 Webhook 注入的字段
POD=$(kubectl get pod -l app=demo -o name | head -1)
kubectl get $POD -o yaml | grep -E 'runtimeClassName|pv-snapshotter|pv-snapshotter--state'

# 3. 在节点上确认 upperdir 指向 PVC
NODE_POD_UID=$(kubectl get $POD -o jsonpath='{.metadata.uid}')
# ssh <node> "findmnt -t overlay | grep $NODE_POD_UID"
# upperdir= 应指向 /var/lib/kubelet/pods/<uid>/volumes/…/upper

# 4. 在容器根文件系统中写入哨兵文件
#    （overlay upperdir 在 PVC 上，该写入在 Pod 重建后会保留）
kubectl exec $POD -c demo -- sh -c 'echo ok > /tmp/sentinel.txt'

# 5. 删除 Pod——ReplicaSet 会对同一 PVC 重建 Pod
kubectl delete $POD
kubectl wait --for=condition=Ready pod -l app=demo --timeout=60s

# 6. 确认文件在 Pod 重建后依然存在
kubectl exec $(kubectl get pod -l app=demo -o name | head -1) -c demo -- \
  cat /tmp/sentinel.txt
# 预期输出：ok
```

**清理：**

```bash
kubectl delete -f demo.yaml
```

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
# 确认 upperdir= 指向提供的路径，而非 /var/lib/containerd/io.containerd.snapshotter.v1.pv-snapshotter/snapshots/...
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

pv-snapshotter **已生产就绪**。核心快照器、Helm chart、containerd 配置自动化以及变更准入 Webhook 均已实现并端到端验证。以下为可选的加固项与未来规划。

### 可选加固

- [ ] `Remove()` 时可配置的清理行为（解除绑定 vs. 回收后端存储）
- [ ] 节点重启恢复端到端验证
- [ ] 存储扩容端到端测试
- ~~GC 协调：overlay metadata.db 清理 vs. 后端存储生命周期~~ — **已在 v0.1.4 修复**

### 未来规划

- [ ] 支持 Ceph RBD globalmount 暂存路径（通过 `sha256(volumeHandle)` 自动路径检测）
- [ ] 多架构镜像构建（arm64）

---

## 升级说明

### 从 v0.1.4 及更早版本升级时迁移 `rootPath`

自 **v0.1.5** 起，快照器根路径默认值从 `/var/lib/containerd` 改为
`/var/lib/containerd/io.containerd.snapshotter.v1.pv-snapshotter`
（Helm 值 `overlaySnapshotter.rootPath` 与 Go 兜底默认值）。

> **保持旧路径则无需迁移。** 新默认值仅影响*默认*取值。若你显式设置
> `overlaySnapshotter.rootPath: /var/lib/containerd`（即 v0.1.4 的取值），
> 升级到 v0.1.5 即为透明的原地升级——pv-snapshotter 继续使用既有的
> `snapshots/` 与 `metadata.db`，下述步骤一概不适用。仅当你*同时*选择采用新的
> 默认路径时，才需要执行下面的迁移。

该变更对**已运行过 v0.1.4 或更早版本的节点并非透明**。pv-snapshotter 把自己的
`snapshots/` 目录与 `metadata.db` 直接放在 `rootPath` 下。若仅改 `rootPath` 后重启：

- 新的 `metadata.db` 从空开始，不认识既有快照。
- 此前解压进 pv-snapshotter 的镜像层会遗留在旧路径下；相关镜像在 `Prepare()` 时
  报 `missing parent snapshot`。
- 运行中的容器仍正常（runc 持有其 overlay 挂载），但新建的使用
  `runtimeClassName: pv` 的 Pod 可能在相关镜像被重新解压前失败。

**推荐迁移方式（在新路径上全新开始）：**

1. cordon 并 drain 节点，确保不再调度 `runtimeClassName: pv` 的 Pod。
2. 先停止 pv-snapshotter，再停止 containerd。
3. 可以把旧数据迁移到新位置，或直接在新路径上全新开始，让 containerd 在
   `CreateContainer` 时按需重新解压镜像（RuntimeClass 路由自动处理，
   Kubernetes 1.29+ 无需手动 `ctr pull`）。
4. 重启 containerd 与 pv-snapshotter；uncordon 节点。

**若节点上残留 v0.1.4 磁盘压力事故遗留的脏元数据**（孤立的 `pv-snapshotter`
BoltDB bucket，或 backing blob 已被 GC 但 image record 仍在的镜像），请使用
[`docs/v0.1.4-recovery-tooling`](https://github.com/humble-mun/pv-snapshotter/tree/docs/v0.1.4-recovery-tooling)
分支下的专用恢复工具：

- `docs/fix-meta/` — 离线 BoltDB 工具，从 containerd 主 `metadata.db` 中删除残留的
  `pv-snapshotter` 快照 bucket（带 SHA-256 校验的备份，默认 dry-run，`--apply` 提交）。
- `docs/prune-images/` — platform-scoped 工具，删除那些本节点平台 config/layer blob
  实际缺失的 image **record**，强制 kubelet 重新拉取（修复“镜像看似存在但无法解压”
  的死锁）。
- `docs/recover-v0.1.4/` — 自包含的恢复 DaemonSet，按节点把上述两个工具串联起来，
  外加一个用于事后清理每节点恢复产物的 cleanup DaemonSet。完整 runbook 见该分支的
  `docs/recover-v0.1.4/README.md`。

---

## 变更日志

### v0.1.7 — 孤立 lease GC、抓取钩子、字符串常量重构（当前版本）

**1. 孤立 lease GC（`dedup.go`：`countOrphanLeases` / `gcOrphanLeases`）**

当 `Remove()` 未能成功释放 lease 时（例如 pv-snapshotter 在释放途中重启），该 lease 成为“孤立 lease”——其 `owner-snapshot` 所对应的活跃快照已不再存在于 `localSn` 中，但 lease 仍然持有，使 overlayfs chainID 持续被 GC 保护。新增两个方法解决此问题：

- `countOrphanLeases(ctx, ns)` — 遍历所有带 `pv-snapshotter.io/managed-by=pv-snapshotter` 标签的 lease，对每个 `owner-snapshot` 调用 `localSn.Stat()` 检查其是否存在，返回孤立 lease 数量。供抓取钩子刷新指标使用。
- `gcOrphanLeases(ctx, ns)` — 同样的遍历逻辑，删除每个孤立 lease，每删除一个调用 `pinnedSnapshotsTotal.Dec()`，返回已删除数量。尽力而为：出错时记录日志并继续扫描。

**2. 新 Prometheus 指标 + `RegisterScrapeHook` 实现**

- 新增 `pv_snapshotter_orphan_leases_total{node_name}` GaugeVec（`service.go`），每次 `/metrics` 抓取时刷新。
- `RegisterScrapeHook(ctx context.Context)` 此前为 TODO 存根，现已实现：调用 `countOrphanLeases` 并更新该指标。通过 `main.go` 中的 `metrics.RegisterScrapeHook(svc.RegisterScrapeHook)` 接入（chassis v0.1.7 API）。
- 新增 `POST /dedup/leases/gc` 接口：触发一次 GC 扫描，返回 `{"deleted": N}`；未启用 dedup 时返回 501。

**3. 字符串常量重构**

快照器包内所有裸字面量 `"k8s.io"` 与 `"kubernetes.io"` 替换为 `resolver.go` 中定义的具名常量：

| 常量名 | 值 |
|--------|-----|
| `containerdNamespaceK8s` | `"k8s.io"` |
| `reservedAnnotationPrefixKubernetes` | `"kubernetes.io"` |
| `reservedAnnotationPrefixK8s` | `"k8s.io"` |

涉及文件：`resolver.go`（常量定义 + `validateAnnotationPrefix`），`service.go`（5 处调用点），`dedup.go`（1 处命名空间兜底赋值）。import 路径与模板字符串片段（`kubernetes.io~csi`）保持不变。

### v0.1.6 — 只读层去重（--share-overlayfs-lowers）+ 修复 pause 容器 upperdir 冲突

**只读层去重（可选，默认关闭）。** 以往 pv-snapshotter 会在自己的 `metadata.db` 中重新解压每一层镜像，与宿主机的原生 overlayfs 形成两份独立副本。启用 `--share-overlayfs-lowers=true`（Helm 值 `overlaySnapshotter.shareOverlayfsLowers: true`）后，pv-snapshotter 在容器准备阶段检测到链路顶层 chainID 仅存在于宿主机 overlayfs 中时，会自动创建"引用快照"——将 `fs/` 目录替换为指向宿主机 overlayfs 层物理路径的符号链接——而非重新解压，从而消除双份只读层的磁盘开销。引用快照通过 containerd lease（不设过期时间）保护宿主机 layer 及其整条父链，防止被 GC 回收；容器 Remove 时自动释放 lease。运维 API：`GET /dedup/leases` 列出所有 pv-snapshotter 管理的 lease，`DELETE /dedup/leases/:leaseID` 手动释放泄漏的 lease，`POST /dedup/leases/gc` 触发孤立 lease GC（返回 `{"deleted": N}`，见 v0.1.7）。Prometheus 指标：`pv_snapshotter_pinned_snapshots_total{node_name}`、`pv_snapshotter_unpin_failures_total{node_name}`（建议对后者设告警）与 `pv_snapshotter_orphan_leases_total{node_name}`（每次抓取刷新，见 v0.1.7）。**启用前必须在目标内核版本上完成 P0-1 至 P0-4 验证**（参见 AGENTS.md），该行为依赖内核将符号链接接受为 lowerdir 条目，属实现细节而非文档化 API。

**修复 pause（sandbox）容器 upperdir 重定向。** 以往 pause 容器（infracontainer）与同 Pod 的业务容器会拿到相同的 `upperdirRoot`，导致两个 overlay mount 共享同一 `workdir`，内核拒绝挂载。现在 pause 容器跳过 upperdir 重定向（pause 不写业务数据），只有业务容器的 overlay mount 才会被重写。

**关键约束**：`leases.WithExpiration(0)` 表示立即过期，而非"永不过期"；创建永久 lease 时必须**省略**该参数，否则 GC 保护立即失效。

### v0.1.5 — 规范化快照器根路径 + 升级指引

- **默认 `rootPath` 迁移**至 `/var/lib/containerd/io.containerd.snapshotter.v1.pv-snapshotter`，
  遵循 `io.containerd.snapshotter.v1.<name>` 命名约定。pv-snapshotter 自己的
  `snapshots/` 与 `metadata.db` 现在位于一个自包含子树中，而非散落在 containerd
  数据目录根部。该变更同时作用于 Helm chart 默认值（`overlaySnapshotter.rootPath`）
  与 Go 兜底默认值。
- **仅适用于全新安装。** 在已有 pv-snapshotter 快照的节点上迁移 `rootPath` 会使旧的
  快照与 `metadata.db` 失联。从 v0.1.4 及更早版本升级时的迁移路径见
  [升级说明](#升级说明)。

### v0.1.4 — 修复 GC 磁盘积累（转发 `Cleanup()` gRPC 调用）

**根本原因。** `syncRemove=false`（默认值）时，`Remove()` 只删除 BoltDB 元数据记录，
实际的 `os.RemoveAll` 推迟到后续 `Cleanup()` 调用时执行。`snapshotservice.FromSnapshotter`
通过对包装器的 `snapshots.Cleaner` 类型断言来分发 gRPC Cleanup RPC。由于包装器结构体
嵌入的是 `snapshots.Snapshotter` 接口而非具体的 `*overlay.snapshotter`，类型断言始终
失败并返回 `ErrNotImplemented` — 导致每个被删除的容器都会在磁盘上留下永久性的孤立目录。
运行 v0.1.3 的节点会无限积累这些目录，最终触发磁盘压力。

**修复。** 引入 `cleanerSnapshotter`，它是 `snapshotter` 的轻量包装，在
`RegisterGRPCService` 启动时做一次类型断言并持有 `snapshots.Cleaner` 引用。
`cleanerSnapshotter.Cleanup()` 直接委托给内部 cleaner，无运行时断言。
若底层快照器未实现 `snapshots.Cleaner`，则使用普通 `snapshotter`（不暴露 `Cleanup`）。

**请勿在生产环境中运行 v0.1.3。** 请升级到 v0.1.4 或更高版本。

---

## 许可证

本项目基于 Apache License 2.0 开源，详见 [LICENSE](LICENSE) 和 [NOTICE](NOTICE)。
