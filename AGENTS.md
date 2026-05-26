# CLAUDE.md

Reference document for AI coding tools (Claude Code, Cursor, Codex, etc.). Covers project purpose, architecture decisions, actual code structure, hard constraints, current status, and next steps. **Read this before making any changes.**

---

## Background

`pv-snapshotter` is a containerd proxy snapshotter that redirects an overlayfs container's writable layer (`upperdir`/`workdir`) onto a path the caller provides — typically a PersistentVolume mounted on the node.

**What it solves:** By default, a container's overlay `upperdir` lives under containerd's snapshot directory and is destroyed when the container sandbox is torn down. Any write made outside an explicitly mounted volume is lost on recreation. `pv-snapshotter` lets that writable layer live on durable storage instead, so writes survive container/pod recreation — with no data copied and no image commit.

**How it does it:** The snapshotter wraps the native overlay snapshotter and overrides only `Mounts()`. When the pod carries the configured upperdir annotation, it rewrites `upperdir=`/`workdir=` to point under the provided path. With no annotation, behavior is identical to native overlayfs.

**Boundary (important):** The snapshotter only rewrites mount options. It does not provision storage, does not call the Kubernetes API or CSI, does not write the annotation, and does not delete the contents of the provided path. Computing the path, mounting the volume, writing the annotation onto the pod, and reclaiming data are all the caller's responsibility (e.g. an operator/controller you provide).

---

## Core Architecture Decisions

### 1. Proxy snapshotter (gRPC plugin) — containerd is not modified

- Serves a gRPC API over a Unix socket; containerd references it via `[proxy_plugins.pv-snapshotter]`
- Uses `snapshotservice.FromSnapshotter` to wrap a `snapshots.Snapshotter` into a gRPC service
- Runs as an independent process — can be deployed and upgraded independently
- Isolated from containerd: a crash here does not take down containerd

### 2. Opt-in via RuntimeClass, not the global default snapshotter

Routing is done via **RuntimeClass**, not per-Pod annotations.

**Key fact:** containerd does not support selecting a snapshotter via a per-Pod annotation. The key `containerd.io/snapshotter` does not exist (an early design misconception). The only per-Pod snapshotter selection mechanism is `RuntimeClass → runtime → snapshotter`.

In practice: add a new runtime entry in the containerd config with `snapshotter = "pv-snapshotter"` and create a matching RuntimeClass. The snapshotter and `runtime_type` are orthogonal — pairing pv-snapshotter with any runtime handler is a TOML config addition, not a code change.

```toml
# A runtime that uses pv-snapshotter (paired here with runc)
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.pv]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "pv-snapshotter"
```

Only Pods with `spec.runtimeClassName: pv` go through this snapshotter.

### 3. Why not the global default snapshotter

- **Bootstrap chicken-and-egg:** pv-snapshotter itself runs as a DaemonSet. If it were the global default, its own Pods would depend on itself.
- **Blast radius:** A fault or upgrade should not affect all Pods on the node.
- **Non-intrusive:** Existing RuntimeClasses and workloads are unaffected.

### 4. DaemonSet deployment

- The DaemonSet's own Pods do **not** set `runtimeClassName: pv` → they use the default overlayfs → no self-dependency
- Workload Pods with `runtimeClassName: pv` → routed through pv-snapshotter
- A pv-snapshotter failure only affects newly created Pods that declared `pv`; existing Pods, the DaemonSet itself, and other workloads are unaffected

### 5. Storage: the upperdir path is backed by a mounted volume

- The provided path is expected to be a single mounted filesystem (commonly a PVC), sized to the workload's needs
- The snapshotter creates two subdirectories under it: `upper/` and `work/` — they **must be on the same filesystem** (hard overlayfs requirement)
- If the volume is a CSI PVC, quota resize follows the standard CSI expansion path: update `PVC.spec.resources.requests.storage` → CSI driver resize → online `resize2fs`/`xfs_growfs` — no container restart required
- **Do not use CephFS as the upperdir backend:** small-file metadata performance is poor; use block devices (RBD, local PV, etc.) formatted as xfs or ext4

### 6. Storage mounting is delegated to the caller; the snapshotter does not manage storage

- A CSI driver's `NodePublishVolume` (or `NodeStageVolume`) mounts the volume to a path on the node
- The Pod declares the volume normally in its PodSpec with a mountPath of your choosing
- pv-snapshotter does not call the K8s API or CSI — it only consumes the mount path already prepared on the node and named in the annotation

**Critical sequencing fact:** kubelet's `WaitForAttachAndMount` completes before CRI `CreateContainer`, so by the time the snapshotter's `Mounts()` is called the volume is already mounted. This is the foundational prerequisite that makes the approach work.

**Pod must declare both `spec.volumes` and `volumeMounts`:** kubelet only triggers CSI NodeStageVolume/NodePublishVolume for volumes actually mounted by a container. A volume declared only in `spec.volumes` with no `volumeMounts` entry is skipped by kubelet — the mount path will not exist. The container mountPath value itself is irrelevant to the snapshotter, but a `volumeMounts` entry is required to trigger the kubelet CSI mount sequence.

**Computing the path (ZFS LocalPV example):**

OpenEBS ZFS LocalPV does not use a globalmount staging path. It mounts directly to:

```
/var/lib/kubelet/pods/<podUID>/volumes/kubernetes.io~csi/<pvName>/mount
```

where:
- `<podUID>` = `Pod.metadata.uid`
- `<pvName>` = `PV.metadata.name` (e.g. `pvc-7cb2f1df-8092-4b89-9f19-d2878aa2d3ec`)

For CSI drivers that do use a staging path (e.g. Ceph RBD), kubelet constructs:

```
/var/lib/kubelet/plugins/kubernetes.io/csi/<driver>/<sha256(volumeHandle)>/globalmount
```

The caller computes the correct path (e.g. from `PVC.spec.volumeName → PV.spec.csi`) and writes it into a Pod annotation. **The snapshotter never calls the K8s API or CSI — it only consumes the path from the annotation.**

### 7. Per-pod configuration via Pod annotations, resolved at `Mounts()` time

#### Why the `containerd.io/snapshot/` prefix cannot be used

containerd CRI's `FilterInheritedLabels` only forwards annotations with the `containerd.io/snapshot/` prefix to the snapshotter's `Prepare()` opts. This conflicts with the Kubernetes annotation key format:

- K8s annotation key format: `prefix/name` — only one `/` allowed
- `containerd.io/snapshot/pv-snapshotter.xxx` contains two `/` — Kubernetes rejects the Pod

Therefore **per-pod config cannot be passed through `Prepare()` opts**. `FilterInheritedLabels` is a dead end.

#### Actual approach: standard K8s annotations + lookup at `Mounts()` time

Use valid K8s annotation keys (single `/`). At `Mounts()` time, use a containerd client to look up the container's metadata and read pod annotations.

**Timing guarantee:** `Mounts()` is always called after the containerd container record is created, whereas `Prepare()` is called before. Therefore the annotation lookup must happen in `Mounts()`, not `Prepare()`.

#### Annotation design

All annotation keys share a configurable DNS subdomain prefix (default: `pv-snapshotter.humble-mun.io`), controlled by the `--annotation-prefix` flag at startup. The prefix is validated as an RFC 1123 DNS subdomain; reserved domains (`kubernetes.io`, `k8s.io`) are rejected.

Three annotation keys are derived from the prefix at startup:

| Key | Purpose |
|-----|---------|
| `<prefix>/upperdir-path` | Literal path to the upperdir root. Non-empty = activate redirection. Takes precedence over the template key. |
| `<prefix>/upperdir-path-template` | Go `text/template` string rendered to produce the upperdir root path. Used when the path contains dynamic fields (e.g. `PodUID`). |
| `<prefix>/var.<VarName>` | Custom template variable. Stripped of the `<prefix>/var.` sub-prefix and injected into the template data map as `VarName`. |

**Template built-in variables** (always available, sourced from sandbox metadata):

| Variable | Value |
|----------|-------|
| `{{.PodUID}}` | `Pod.metadata.uid` |
| `{{.PodName}}` | `Pod.metadata.name` |
| `{{.PodNamespace}}` | `Pod.metadata.namespace` |

**Example annotation set (path backed by a ZFS LocalPV volume):**

```yaml
annotations:
  pv-snapshotter.humble-mun.io/upperdir-path-template: >-
    /var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount
  pv-snapshotter.humble-mun.io/var.PVName: pvc-7cb2f1df-8092-4b89-9f19-d2878aa2d3ec
```

The `var.` sub-prefix explicitly marks template variables, separating them from control keys. This avoids hardcoded exclusion lists and makes future additions safe.

#### Lookup chain at `Mounts(key)` time

```
sandbox   Mounts(key="k8s.io/N/sandboxID"):
  → containerd client.Containers(nsCtx, "id==sandboxID")
  → container.Extensions()["io.cri-containerd.sandbox.metadata"]
  → json.Unmarshal → Metadata.Config.annotations["<prefix>/upperdir-path"]
                   → Metadata.Config.annotations["<prefix>/upperdir-path-template"]
                   → Metadata.Config.metadata.uid / .name / .namespace

workload  Mounts(key="k8s.io/N/workloadID"):
  → containerd client.Containers(nsCtx, "id==workloadID")
  → container.Info().SandboxID
  → same as sandbox path above
```

The containerd metadata filter adaptor supports only `id`, `runtime.name`, `image`, `labels.<key>` (`snapshot_key` is silently ignored and always returns empty). Since CRI sets `SnapshotKey == container.ID`, filter with `id==<containerID>`.

**sandbox metadata JSON structure** (content of `io.cri-containerd.sandbox.metadata` extension):

```json
{
  "Version": "v1",
  "Metadata": {
    "Config": {
      "metadata": { "name": "pod-name", "uid": "1834d42c-...", "namespace": "default" },
      "annotations": {
        "pv-snapshotter.humble-mun.io/upperdir-path-template": "...",
        "pv-snapshotter.humble-mun.io/var.PVName": "pvc-..."
      }
    }
  }
}
```

Local deserialization structs (no dependency on containerd internal packages):

```go
type sandboxMetadata struct {
    Metadata sandboxMetadataInner `json:"Metadata"`
}
type sandboxMetadataInner struct {
    Config *podSandboxConfig `json:"Config"`
}
type podSandboxConfig struct {
    Metadata    *podObjectMeta    `json:"metadata"`
    Annotations map[string]string `json:"annotations"`
}
type podObjectMeta struct {
    Name      string `json:"name"`
    UID       string `json:"uid"`
    Namespace string `json:"namespace"`
}
```

### 8. Routing decision in `Mounts()`, `Prepare()` is a full pass-through

**`Prepare()`:** Full pass-through to native overlay. Creates snapshot directories (`upper/`, `work/`) under the snapshotter root and writes BoltDB metadata. No Linux mount syscalls, no upperdir setup.

**`Mounts()`:** Calls native overlay's `Mounts()` to get the default `[]mount.Mount`, then resolves the annotation. If an upperdir path is found:
1. `ensureUpperdirReady(upperdirRoot)` — validates the path is an existing mountpoint, creates `upper/` and `work/` subdirectories
2. `replaceUpperdirOptions(mounts, upperdirRoot)` — rewrites `upperdir=` and `workdir=` options to point at `upperdirRoot/upper` and `upperdirRoot/work`; `lowerdir=` and all other options are preserved

`Mounts()` returns a data structure; the actual mount syscall is performed by runc. Replacing options in the returned struct before runc executes it is entirely correct.

The native snapshot directories created by `Prepare()` under the snapshotter root are never used (because `Mounts()` redirected elsewhere), but they are cleaned up normally by the native overlay on `Remove()`.

**Routing guard — `ensureUpperdirReady` fails hard, no fallback:**

If the upperdir path is not a mountpoint (volume not yet ready), `Mounts()` returns an error. The Pod fails to start. This is intentional: silently falling back to native overlay would cause undetected state loss.

**`Commit` / `Remove` / `Stat` / `Walk`:** Pass-through only. They operate on containerd metadata, not filesystem paths.

---

## Code Layout

```
pv-snapshotter/
├── cmd/
│   └── daemon/                  # main entrypoint
│       ├── main.go              # root command (daemon gRPC server)
│       └── config.go            # "config" subcommand (sidecar lifecycle)
├── pkg/
│   ├── containerd/
│   │   ├── config/              # containerd config patching + sidecar support (all files: //go:build linux)
│   │   │   ├── flags.go         # flag constants, RegisterFlags, GetParams, GetSocketPath
│   │   │   ├── nsenter.go       # RestartContainerd via cgo constructor + re-exec
│   │   │   ├── patcher.go       # Apply: TOML surgical edit, base-runtime clone
│   │   │   └── wait.go          # WaitUntilReady: HTTP /readyz over Unix socket
│   │   └── snapshotter/         # core snapshotter implementation (all files: //go:build linux)
│   │       ├── service.go       # flags, gRPC service registration, all snapshots.Snapshotter method overrides
│   │       ├── overlay.go       # native overlay snapshotter construction and config struct
│   │       ├── mount.go         # ensureUpperdirReady, replaceUpperdirOptions
│   │       └── resolver.go      # annotation lookup via containerd client at Mounts() time
│   └── service/
│       └── common.go            # service name constant
├── charts/
│   └── pv-snapshotter/          # Helm chart
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
│           ├── daemonset.yaml   # ConfigMap + DaemonSet (daemon + config sidecar)
│           ├── rbac.yaml        # ServiceAccount (automountServiceAccountToken: false)
│           └── runtimeclass.yaml
├── Dockerfile                   # CGO_ENABLED=1, distroless/base-debian13
├── Makefile
├── go.mod
├── go.sum
├── README.md / README_CN.md
└── AGENTS.md
```

All files under `pkg/containerd/snapshotter/` carry `//go:build linux` — the entire package is Linux-only.

The module depends on `github.com/humble-mun/chassis` for application bootstrap (flags, logging, gRPC/HTTP servers, version info).

---

## Key Code Details

### snapshotter struct (service.go)

```go
type snapshotter struct {
    logger   logr.Logger
    resolver *resolver
    snapshots.Snapshotter          // embedded — all unoverridden methods delegate to native overlay
}
```

No `pvBacked` field. There is no separate branch snapshotter. The redirected path is purely a mount options rewrite in `Mounts()`.

### CLI flags (service.go + resolver.go)

| Flag | Default | Description |
|------|---------|-------------|
| `--unix-socket-path` | `/var/run/pv-snapshotter/daemon.sock` | gRPC listener socket |
| `--containerd-socket` | `/run/containerd/containerd.sock` | containerd client socket |
| `--annotation-prefix` | `pv-snapshotter.humble-mun.io` | Pod annotation DNS subdomain prefix; all three annotation keys derived from this at startup |
| `--overlay-snapshotter.root-path` | `/var/lib/containerd` | Native overlay snapshotter root |
| `--overlay-snapshotter.upper-dir-label` | `false` | Enable `containerd.io/snapshot/overlay.upperdir` label on snapshots |
| `--overlay-snapshotter.sync-remove` | `false` | Synchronous snapshot removal |
| `--overlay-snapshotter.slow-chown` | `false` | Slow chown for ID-mapped mounts |
| `--overlay-snapshotter.mount-options` | `[]` | Extra mount options passed to overlayfs; **never add `volatile`** |

### resolver struct (resolver.go)

```go
type resolver struct {
    client       *containerd.Client
    logger       logr.Logger
    keyLiteral   string  // <prefix>/upperdir-path
    keyTemplate  string  // <prefix>/upperdir-path-template
    keyVarPrefix string  // <prefix>/var.
}
```

All three keys are derived once at startup from `--annotation-prefix`. The resolver connects to containerd at construction time and keeps the connection open.

**Context rule:** Always use `context.Background()` with `namespaces.WithNamespace()` for containerd client calls — never reuse the incoming gRPC server context. The server-side context carries gRPC incoming metadata that must not bleed into outgoing client calls.

### ensureUpperdirReady (mount.go)

1. `os.Stat(upperdirRoot)` — confirm path exists
2. `syscall.Stat` on `upperdirRoot` and its parent — compare `Dev` fields; different device ID = mountpoint
3. `os.MkdirAll` for `upper/` and `work/` — idempotent; succeeds if already exists

### replaceUpperdirOptions (mount.go)

- No-op if: mounts is empty, `mounts[0].Type != "overlay"`, or no `upperdir=` in options (read-only snapshot)
- Replaces `upperdir=*` → `upperdir=<root>/upper`, `workdir=*` → `workdir=<root>/work`
- Returns a new slice; the original is not modified

### config struct (overlay.go)

```go
type config struct {
    RootPath      string   `mapstructure:"root-path"`
    UpperDirLabel bool     `mapstructure:"upper-dir-label"`
    SyncRemove    bool     `mapstructure:"sync-remove"`
    SlowChown     bool     `mapstructure:"slow-chown"`
    MountOptions  []string `mapstructure:"mount-options"`
}
```

`UpperDirLabel` defaults to `false` (same as containerd native). `MountOptions` defaults to empty. **Never add `volatile` to MountOptions** — it causes upperdir data loss on unclean shutdown, directly contradicting the persistence semantics of this project.

### Snapshot key format

```
k8s.io/<seq>/<containerID>
```

This key is the unique identifier for a container's snapshot lifecycle. It is present in all `Prepare`, `Mounts`, `Commit`, `Remove` calls and is logged as `"key"` in every log statement — use it to correlate all log lines for a single container across `service.go` and `resolver.go`.

Image layer unpack keys (`k8s.io/<seq>/extract-<id> sha256:<hash>`) are explicitly filtered out in `parseSnapshotKey` and skip the annotation resolution path entirely.

### Logging conventions

- All log statements include `"key", key` as the first structured field for log correlation
- `V(4)`: normal pod lifecycle events (method call/complete, resolver internal state transitions, redirect routing nodes) — silent at production log levels
- `V(5)`: "decoded sandbox metadata" only — raw JSON dump, intentionally kept at a higher verbosity
- `Error(...)`: all error paths — always emitted regardless of verbosity
- Logger names: `"snapshotter"` (service.go), `"snapshotter.resolver"` (resolver.go)

### containerd v2 import paths

```
github.com/containerd/containerd/v2/client
github.com/containerd/containerd/v2/core/snapshots
github.com/containerd/containerd/v2/core/mount
github.com/containerd/containerd/v2/contrib/snapshotservice
github.com/containerd/containerd/v2/pkg/namespaces
github.com/containerd/containerd/v2/plugins/snapshots/overlay
github.com/containerd/containerd/v2/plugins/snapshots/overlay/overlayutils
```

v1.x has no `/v2` suffix and uses `snapshots/overlay` instead of `plugins/snapshots/overlay`.

---

## Things You Must NOT Do

- **Do not** call `plugin.Register` — that is for in-process embedded plugins; proxy plugins register via the gRPC path.
- **Do not** manually declare the rebase capability — `snapshotservice.FromSnapshotter` exposes it automatically via reflection.
- **Do not** add `volatile` to MountOptions — causes upperdir data loss on unclean shutdown.
- **Do not** use CephFS as the upperdir backend — small-file metadata performance is poor.
- **Do not** modify the containerd default snapshotter (`[plugins."io.containerd.grpc.v1.cri".containerd].snapshotter`) — pv-snapshotter is introduced exclusively via RuntimeClass.
- **Do not** let pv-snapshotter delete the contents of the provided path — on `Remove`, it only cleans up the native snapshot dir. The backing storage lifecycle is owned by the caller.
- **Do not** call the K8s API or CSI in `Prepare()` or `Mounts()` — pass information via annotations; avoid API latency on the critical path.
- **Do not** reuse the incoming gRPC server context for containerd client calls — always use `context.Background()` + `namespaces.WithNamespace()`.
- **Do not** filter containerd containers by `snapshot_key` — `adaptContainer` silently ignores it; filter by `id` instead.
- **Do not** silently fall back to native overlay on `ensureUpperdirReady` failure — fail hard; silent fallback causes undetected state loss.
- **Do not** add a `pvBacked snapshots.Snapshotter` field to the snapshotter struct — the redirected path is purely a mount options rewrite, not a separate snapshotter branch.

---

## Go Code Style

- Use named return values
- Error wrapping: `fmt.Errorf("context: %w", err)` to preserve the chain
- Logging: `go-logr/logr` (not `klog` directly). Logger name conventions: `"snapshotter"`, `"snapshotter.resolver"`
- All structured log fields use the `"key", value` pattern; never `fmt.Sprintf` structured fields
- All files in `pkg/containerd/snapshotter/` must carry `//go:build linux`
- All files in `pkg/containerd/config/` must carry `//go:build linux`

---

## Current Status

### Done — Phase 1: Pure pass-through + logging

`snapshotservice.FromSnapshotter` wraps `overlay.NewSnapshotter`. All snapshotter interface methods are overridden with logging. End-to-end verified:

- containerd recognizes the `pv-snapshotter` proxy plugin
- `ctr --snapshotter=pv-snapshotter` command chain works
- Snapshot key format confirmed: `k8s.io/<seq>/<containerID>`
- Method call sequence confirmed: `Prepare → Commit (per layer) → Prepare (top) → Mounts (multiple) → Remove`

### Done — Phase 2a: Annotation resolution chain

Full annotation propagation path implemented and verified end-to-end:

- Confirmed `containerd.io/snapshot/` prefix path is not viable (K8s rejects double-slash keys)
- Confirmed routing must happen in `Mounts()` not `Prepare()` (container record does not exist at `Prepare()` time)
- Confirmed `snapshot_key` cannot be used as a filter field
- Confirmed `context.Background()` is required for containerd client calls
- `resolver.go` implemented: sandbox container extension lookup, JSON deserialization, annotation reading
- Both sandbox path and workload → sandbox path verified working

### Done — Phase 2b: upperdir replacement

upperdir redirection fully implemented and verified:

- `ensureUpperdirReady`: mountpoint check via `syscall.Stat` device ID comparison, `upper/` and `work/` directory creation
- `replaceUpperdirOptions`: rewrites `upperdir=` and `workdir=` in overlay mount options
- End-to-end verified with ZFS LocalPV: the container's overlay mount confirms `upperdir` points to the provided path
- State persistence verified: write a file → delete the pod → recreate a pod referencing the same backing path → file is preserved

### Annotation design: template support added

Beyond the original literal `upperdir-path`, a `upperdir-path-template` key was added to support dynamic path construction. The `--annotation-prefix` flag makes the annotation namespace configurable.

The caller writes the resolved path into the annotation. For ZFS LocalPV (no globalmount staging path), the path is:

```
/var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount
```

### Done — Phase 3: Helm chart + containerd config automation (CURRENT STATE)

Full DaemonSet deployment package implemented:

- **`pkg/containerd/config/`** — containerd config patching package:
  - `patcher.go`: idempotent TOML surgical append; clones base runtime's config subtree (preserving `SystemdCgroup`, `BinaryName`, etc.) and overrides `snapshotter = "pv-snapshotter"`; detects existing sections via `map[string]any` parse, never re-serialises the whole tree
  - `nsenter.go`: `RestartContainerd()` via cgo `__attribute__((constructor))` + re-exec pattern; the C constructor runs before Go runtime threads start, so `setns(CLONE_NEWNS)` succeeds; gated by `_PV_NSENTER` env var
  - `wait.go`: `WaitUntilReady()` polls `HTTP GET /readyz` over Unix socket; verbosity decreases from `V(10)` toward `V(1)` as retry count increases
  - `flags.go`: `RegisterFlags` + `GetParams` + `GetSocketPath`
- **`cmd/daemon/config.go`** — `config` subcommand lifecycle:
  1. `WaitUntilReady`: polls daemon `/readyz` via Unix socket HTTP
  2. `Apply`: patch `config.toml`, clone base runtime config
  3. `RestartContainerd`: if modified, re-exec via cgo nsenter preamble
  4. `<-ctx.Done()`: block for Pod lifetime as native sidecar
- **`charts/pv-snapshotter/`** — complete Helm chart; see Deployment section for details

---

## Next Steps

### Production hardening

- **Cleanup behavior on `Remove`:** The caller may want "forget the binding" (keep backing data) vs. "reclaim" (also clean up backing data). The snapshotter does not decide this itself; a configurable/signaled behavior could be added, but reclaiming backing storage remains the caller's responsibility.
- **Node restart recovery:** After pv-snapshotter restarts, existing running containers still have their overlay mounts active (runc holds them). Verify that re-created pods referencing the same backing path correctly re-attach on next `Mounts()` call.
- **Storage expansion:** Update `PVC.spec.resources.requests.storage` → CSI driver resizes block device → `resize2fs`/`xfs_growfs` online. No container restart required. Verify the container sees new space.
- **Error recovery:** mount failure, missing parent snapshot, volume not yet ready when `Mounts()` is called.
- **GC coordination:** overlay metadata.db cleanup vs. backing storage lifecycle.

### Evolution principles

Each phase is a small increment. Any phase can revert to pure pass-through. New code must maintain the invariant: **when no upperdir annotation is present, behavior is identical to native overlayfs.**

---

## Deployment and Configuration

### containerd config (`/etc/containerd/config.toml`)

```toml
[proxy_plugins.pv-snapshotter]
  type = "snapshot"
  address = "/var/run/pv-snapshotter/daemon.sock"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.pv]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "pv-snapshotter"
```

**Do not touch** `[plugins."io.containerd.grpc.v1.cri".containerd].snapshotter`.

### RuntimeClass

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: pv
handler: pv
```

### DaemonSet requirements

- `hostPath` mount for `/var/run/pv-snapshotter/` — socket address must match containerd config
- `hostPath` mount for `/run/containerd/` (**directory**, not the socket file) — resolver needs this to query container metadata; mounting the directory keeps the path valid across containerd restarts (a file-level bind-mount becomes stale when containerd deletes and recreates the socket)
- `hostPath` mount for `/var/lib/kubelet` — required for the CSI mount path to be accessible
- The DaemonSet's own Pods **must not** set `runtimeClassName: pv`
- Use `nodeSelector` or tolerations to restrict to target nodes

### Startup ordering

containerd connects to the proxy plugin at startup. If pv-snapshotter is not running, containerd marks the plugin unhealthy and does **not** automatically reconnect.

- systemd: use `After=` / `Requires=` to ensure pv-snapshotter starts before containerd
- DaemonSet upgrade: cordon → drain workload Pods → restart pv-snapshotter + containerd → uncordon

### Failure semantics

If a Pod declares `runtimeClassName: pv` but the pv-snapshotter socket is unavailable, containerd errors and the Pod fails to start — it **will not** silently fall back to overlayfs. This is intentional.

---

## Operational Constraints

- **Backing path inside the Pod:** If the volume is also mounted inside the Pod, use a dedicated mountPath and prevent workloads from writing to it directly — it is the raw upper directory. Enforce with LSM (AppArmor / SELinux) if needed.
- **Data lifecycle:** The component that owns the backing storage distinguishes "keep" vs. "reclaim" and acts accordingly. The snapshotter never deletes backing data.
- **`nerdctl commit`** works against a container backed by this snapshotter, like any other:
  ```bash
  nerdctl --namespace=k8s.io --snapshotter=pv-snapshotter commit <container> <image>
  ```

---

## Debugging and Verification

### Verify proxy plugin registration

```bash
ctr plugins ls | grep pv-snapshotter
# Expected: io.containerd.snapshotter.v1  pv-snapshotter  ok
```

### Test directly with ctr (bypass K8s and CRI)

```bash
ctr --namespace=k8s.io snapshots --snapshotter=pv-snapshotter ls
ctr --namespace=k8s.io run --snapshotter=pv-snapshotter --rm -t docker.io/library/alpine:latest test sh
```

### Confirm the redirected upperdir is active

Inside the container or via `findmnt` on the node:

```bash
# On node — find the container's overlay mount
findmnt -t overlay
# Confirm upperdir= points to the provided path, not /var/lib/containerd/snapshots/...
```

### Confirm state persistence

1. Write a file inside the container
2. Delete the pod (its writable layer would normally be lost)
3. Recreate a pod referencing the same backing path
4. Verify the file is still present

### Log verbosity for debugging

```bash
# Show all pod lifecycle events (method calls, resolver steps, redirect routing)
--v=4

# Also show raw sandbox metadata JSON
--v=5

# Correlate all logs for a specific container
grep 'key="k8s.io/31/4856f54d' /var/log/pv-snapshotter.log
```

### Inspect snapshot info

```bash
ctr --namespace=k8s.io snapshots --snapshotter=pv-snapshotter info <key>
# Labels field shows containerd.io/snapshot/overlay.upperdir when UpperDirLabel=true
```

---

## References

### containerd source

- v2.x: `plugins/snapshots/overlay/overlay.go`, `plugins/snapshots/overlay/plugin/plugin.go`
- Proxy plugin integration: `contrib/snapshotservice/`
- Overlay support detection: `plugins/snapshots/overlay/overlayutils/`

### Documentation

- containerd Plugins: `docs/PLUGINS.md`
- CRI config: `docs/cri/config.md`

### Similar projects (proxy snapshotter pattern)

- `containerd/stargz-snapshotter` (lazy pull)
- `containerd/nydus-snapshotter` (RAFS-based lazy pull)
- `awslabs/soci-snapshotter` (AWS lazy pull)

### Related containerd issues / PRs

- #6657: per-pod snapshotter selection (confirms RuntimeClass is the only mechanism)
- #6899: runtime-level snapshotter implementation
- #9361: rebase capability and parallel layer unpack

---

## One-sentence summary

**The snapshotter does exactly one thing: when a Pod has an upperdir annotation, rewrite the overlay mount options returned to runc so that `upperdir=` and `workdir=` point to `upper/` and `work/` inside the caller-provided path. Everything else is a pass-through to native overlayfs.**
