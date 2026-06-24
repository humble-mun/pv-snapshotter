# pv-snapshotter

English | [简体中文](README_CN.md)

> ⚠️ **DO NOT USE v0.1.3 IN PRODUCTION.**  v0.1.3 has a confirmed disk-pressure
> bug: orphaned snapshot directories accumulate indefinitely under
> `/var/lib/containerd/snapshots/` because `Cleanup()` is never forwarded to the
> underlying overlay snapshotter.  Nodes will eventually hit disk pressure and go
> NotReady.  Upgrade to **v0.1.4 or later** which forwards the `Cleanup()` gRPC
> call and reclaims orphaned directories.

A containerd proxy snapshotter that redirects an overlayfs container's writable layer (`upperdir`/`workdir`) to a caller-provided path — for example, a mounted PersistentVolume — so that writes made outside mounted volumes can land on durable storage with zero data-copy overhead.

**Production ready.** Core snapshotter, Helm chart, containerd config automation, and mutating admission webhook are all implemented and verified end-to-end.

## Table of Contents

- [Overview](#overview)
- [How It Works](#how-it-works)
- [Architecture](#architecture)
- [Getting Started](#getting-started)
  - [Prerequisites](#prerequisites)
  - [Build](#build)
  - [containerd Configuration](#containerd-configuration)
  - [RuntimeClass](#runtimeclass)
  - [Pod Annotation Reference](#pod-annotation-reference)
- [Mutating Admission Webhook](#mutating-admission-webhook)
  - [What the Webhook Does](#what-the-webhook-does)
  - [Annotation Template Pipeline](#annotation-template-pipeline)
  - [Webhook Prerequisites](#webhook-prerequisites)
- [DaemonSet Deployment](#daemonset-deployment)
- [Image Pull Requirement](#image-pull-requirement)
- [Helm Chart](#helm-chart)
- [CLI Flags](#cli-flags)
- [End-to-End Validation](#end-to-end-validation)
- [Observability](#observability)
- [Operational Notes](#operational-notes)
- [Roadmap](#roadmap)
- [License](#license)

---

## Overview

`pv-snapshotter` wraps containerd's native overlayfs snapshotter and overrides exactly one method: `Mounts()`. When a container's pod carries the configured upperdir annotation, the snapshotter rewrites the `upperdir=` and `workdir=` overlay mount options to point under a path the caller supplies. When the annotation is absent, every call is a transparent pass-through to native overlayfs.

The effect is that the container's writable layer lives on storage the caller controls (typically a PersistentVolume mounted on the node) instead of containerd's default snapshot directory — with no image commit, no registry round-trip, and no data copied on container start or stop.

### Scope and Boundaries

The snapshotter does one thing and deliberately stays out of everything else:

- **It does** rewrite the overlay `upperdir=`/`workdir=` options to `<provided-path>/upper` and `<provided-path>/work` when the annotation is present, and pass through to native overlayfs otherwise.
- **It does not** call the Kubernetes API or any CSI driver. It only reads a path from a pod annotation that the caller has already populated.
- **It does not** provision or mount storage. The provided path must already be a ready mountpoint by the time `Mounts()` runs; if it is not, the mount fails hard rather than silently falling back.
- **It does not** delete or garbage-collect the contents of the provided path. `Remove()` only cleans up the native snapshot directories; the lifecycle of the backing storage belongs to whoever created it.

Everything beyond mount-option rewriting — provisioning volumes, computing the path, writing the annotation onto the pod, and deciding when to reclaim data — is the responsibility of the caller (for example, an operator or controller that you provide).

---

## How It Works

```
kubelet                      containerd                   pv-snapshotter
  │                              │                              │
  │── attach & mount volume ──►  │  (CSI mounts the volume)    │
  │                              │                              │
  │── CRI CreateContainer ──►    │── Prepare(key) ──────────►  │ pass-through to native overlay
  │                              │                              │
  │── CRI StartContainer ──►     │── Mounts(key) ───────────►  │ 1. get native overlay mounts
  │                              │                              │ 2. look up pod annotations via
  │                              │                              │    the containerd client
  │                              │                              │ 3. if the annotation is present:
  │                              │                              │    rewrite upperdir= / workdir=
  │                              │◄── []mount.Mount ──────────  │    to the provided path
  │                              │                              │
  │                         runc executes the overlay mount
  │                         (upperdir now on the provided path)
```

**Sequencing guarantee:** kubelet completes volume attachment and mounting before CRI `CreateContainer`, so by the time `Mounts()` is called the volume is already mounted on the node. No race condition.

---

## Architecture

### Proxy Snapshotter (gRPC plugin)

- Serves the containerd snapshots gRPC API on a Unix socket
- Registered in containerd via `[proxy_plugins.pv-snapshotter]` — no containerd modification required
- Wraps the native overlayfs snapshotter; all methods delegate by default
- Only `Mounts()` is modified: it rewrites overlay mount options when the upperdir annotation is present

### Opt-in via RuntimeClass

containerd selects a snapshotter per runtime, not per pod annotation. The only per-pod mechanism is `RuntimeClass → runtime → snapshotter`. Define a containerd runtime that uses `pv-snapshotter` and a matching RuntimeClass; only pods that set that `runtimeClassName` go through it.

```
Pod.spec.runtimeClassName: pv
  └─► RuntimeClass handler: pv
        └─► containerd runtime: pv
              └─► snapshotter: pv-snapshotter
```

The snapshotter is orthogonal to `runtime_type`, so it can be paired with any runtime handler (plain `runc`, a GPU runtime, etc.). Existing workloads and RuntimeClasses are untouched.

### Caller-provided Upperdir Path

The path the writable layer is redirected to is supplied by the caller through a pod annotation. The snapshotter reads it at `Mounts()` time and never queries the Kubernetes API or CSI.

A common source is a CSI volume mounted on the node. For example, OpenEBS ZFS LocalPV (which uses no globalmount staging path) mounts directly to:

```
/var/lib/kubelet/pods/<podUID>/volumes/kubernetes.io~csi/<pvName>/mount
```

How you compute and write that path is entirely up to you — see [Pod Annotation Reference](#pod-annotation-reference).

### Annotation Resolution

The snapshotter reads annotations from the containerd sandbox container extension `io.cri-containerd.sandbox.metadata`. Workload containers look up their parent sandbox by `SandboxID`.

Three annotation keys (all derived from `--annotation-prefix`):

| Key | Purpose |
|-----|---------|
| `<prefix>/upperdir-path` | Literal path to the upperdir root. Takes precedence. |
| `<prefix>/upperdir-path-template` | Go `text/template` rendered to produce the path. |
| `<prefix>/var.<VarName>` | Custom variable injected into the template. |

Built-in template variables: `{{.PodUID}}`, `{{.PodName}}`, `{{.PodNamespace}}`.

**Example (path backed by a ZFS LocalPV volume):**

```yaml
annotations:
  pv-snapshotter.humble-mun.io/upperdir-path-template: >-
    /var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount
  pv-snapshotter.humble-mun.io/var.PVName: pvc-7cb2f1df-8092-4b89-9f19-d2878aa2d3ec
```

---

## Getting Started

### Prerequisites

| Component | Version |
|-----------|---------|
| Linux kernel | ≥ 5.11 (overlayfs on non-root userspace) |
| containerd | v2.x |
| Kubernetes | v1.27+ |
| Go (build only) | 1.26+ |
| CSI driver | Any driver that mounts block storage (RBD, ZFS LocalPV, local PV…) |

> **Do not use CephFS** as the upperdir backend — small-file metadata performance is poor. Use block devices formatted as ext4 or xfs.

### Build

```bash
# Build Docker image (amd64, release)
make build

# Custom arch / repo
make build ARCH=arm64 REPO=my-registry/pv-snapshotter VERSION=v0.1.0

# Debug build (with DWARF symbols)
make build DEBUG=true
```

The resulting image is based on `gcr.io/distroless/base-debian13` (glibc only, no shell, minimal attack surface). CGO is enabled to support the cgo nsenter preamble used for containerd restart.

### containerd Configuration

Add the proxy plugin and a runtime entry to `/etc/containerd/config.toml`:

```toml
# Register the proxy snapshotter
[proxy_plugins.pv-snapshotter]
  type    = "snapshot"
  address = "/var/run/pv-snapshotter/daemon.sock"

# A runtime that uses pv-snapshotter (paired here with runc)
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.pv]
  runtime_type = "io.containerd.runc.v2"
  snapshotter  = "pv-snapshotter"
```

The `snapshotter` and `runtime_type` settings are orthogonal: to pair pv-snapshotter with a different runtime handler, keep `snapshotter = "pv-snapshotter"` and set `runtime_type`/`options` to that handler's values.

> **Do not** modify `[plugins."io.containerd.grpc.v1.cri".containerd].snapshotter`. pv-snapshotter is introduced exclusively via RuntimeClass.

Restart containerd after editing. pv-snapshotter must be running before containerd starts — see [DaemonSet Deployment](#daemonset-deployment) for ordering.

### RuntimeClass

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: pv
handler: pv
```

Apply with:

```bash
kubectl apply -f runtimeclass.yaml
```

Pods opt in by setting:

```yaml
spec:
  runtimeClassName: pv
```

### Pod Annotation Reference

The annotations below must be computed and written onto the pod by the caller (an operator, controller, or admission webhook) before the pod is created. The snapshotter only consumes them.

> **If you use the Helm chart with `webhook.enabled=true`** (the default), the webhook computes and injects these annotations automatically — you do not write them yourself. See [Mutating Admission Webhook](#mutating-admission-webhook).

```yaml
metadata:
  annotations:
    # Option 1 — literal path (highest precedence)
    pv-snapshotter.humble-mun.io/upperdir-path: /var/lib/kubelet/pods/abc123/volumes/kubernetes.io~csi/pvc-xyz/mount

    # Option 2 — Go template (rendered at Mounts() time)
    pv-snapshotter.humble-mun.io/upperdir-path-template: >-
      /var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount
    pv-snapshotter.humble-mun.io/var.PVName: pvc-7cb2f1df-8092-4b89-9f19-d2878aa2d3ec
```

Pods without either annotation pass through transparently to native overlayfs.

---

## Mutating Admission Webhook

The webhook is enabled by default (`webhook.enabled=true` in `values.yaml`). It provides an out-of-the-box experience: workloads that carry an opt-in label are automatically injected with the state volume, annotations, and pv-backed RuntimeClass — no manual annotation authoring required.

### What the Webhook Does

When a Pod is admitted and its labels match the configured `objectSelector` (default: `pv-snapshotter.humble-mun.io/inject: "true"`), the webhook:

1. **Resolves the controlling owner** by traversing owner references up to `maxOwnerDepth` hops (default 2: pod → ReplicaSet → Deployment). The resolved name becomes `OwnerName`.
2. **Looks up the associated PVC**, then waits up to `boundTimeout` (default 10 s) for it to reach Bound. Denies the pod if the PVC does not bind in time — pv-snapshotter cannot prepare the overlay upperdir on an unbound volume, so admitting early only defers the failure to the node. The PVC name is resolved in this order:
   - **Per-pod override**: if the pod sets the `<annotation-prefix>/pvc-name-template` annotation (e.g. `pv-snapshotter.humble-mun.io/pvc-name-template`; the prefix is the configurable `--annotation-prefix`, the suffix is fixed), its value is rendered as a Go template (same variables as `pvcNameTemplate`) and used as the PVC name. This lets a pod bind a PVC whose lifecycle is independent of the pod/owner name (e.g. selecting one of several pre-provisioned PVCs at launch). The value may also be a literal PVC name with no template actions. A rendered-empty value is rejected rather than falling back.
   - **Global name template**: otherwise `pvcNameTemplate` (default `{{.OwnerName}}`) is rendered and the PVC fetched by that name.
   - **Global selector template**: when `pvcNameTemplate` renders empty, `pvcSelectorTemplate` is used to list PVCs and the first match is taken.
3. **Fetches the backing PV** and extracts `spec.csi.volumeHandle` (empty for non-CSI PVs).
4. **Builds a JSON Patch** that:
   - Stamps `upperdir-path-template` and `var.PVName` annotations onto the pod.
   - Appends volume `pv-snapshotter--state` (backed by the PVC) to `spec.volumes`.
   - Adds a `volumeMount` at `/.platform/state` only to the primary container (`spec.containers[0]`).
   - Rewrites `runtimeClassName` to `<base>-pv` (e.g. `runc-pv`), using `defaultRuntimeClass` when the pod has none.

The volume name `pv-snapshotter--state` uses the double-dash vendor separator to avoid colliding with user-defined volume names. The injected mount exists only to make kubelet publish the PVC before container creation; workloads should not read or write that path, and sidecars/init containers do not receive it.

### Annotation Template Pipeline

Annotations are rendered through a three-layer pipeline:

| Layer | Renderer | Variables | Purpose |
|-------|----------|-----------|---------|
| 1 | Helm | `values.yaml` | Renders `--webhook-annotation-templates` CLI arg |
| 2 | Webhook | `.PVName`, `.VolumeHandle`, `.OwnerName`, `.PodName` | Resolves storage-side values; stamps annotation onto pod |
| 3 | pv-snapshotter | `.PodUID`, `.PodName`, `.PodNamespace`, `var.*` | Resolves pod-identity values at `Mounts()` time |

The default `upperdir-path-template` value:

```
/var/lib/kubelet/pods/{{.PodUID}}/volumes/kubernetes.io~csi/{{.PVName}}/mount
```

- `{{.PVName}}` is resolved by the webhook (Layer 2) and substituted with the actual PV name.
- `{{.PodUID}}` passes through Layer 2 unchanged and is resolved by pv-snapshotter (Layer 3).

The `var.PVName` annotation is also stamped with the resolved PV name, making it available to any custom Layer-3 template that references `{{.PVName}}`.

> **Note on `webhook-annotation-templates` configuration:** The `annotationTemplates` field in `values.yaml` is illustrative — it documents the default template text and the three-layer pipeline. The Helm chart renders these values as a `--webhook-annotation-templates=...` CLI argument on the daemon container rather than writing them to `daemon.yaml`. This is intentional: viper's YAML parser lowercases all map keys (`var.PVName` → `var.pvname`), which corrupts annotation key casing. Passing the flag on the command line routes it through pflag's CSV parser, which preserves casing exactly.

### Webhook Prerequisites

- **cert-manager** must be installed in the cluster to issue the webhook TLS certificate.
- A `ClusterIssuer` named `selfsigned` must exist (configurable via `webhook.clusterIssuerName`).
- The webhook listens on port 9443 (configurable via `webhook.bindAddress`).

---

## DaemonSet Deployment

The DaemonSet runs `pv-snapshotter` on each node. Its own Pods **must not** set `runtimeClassName: pv` — otherwise the snapshotter would depend on itself to start.

Required `hostPath` mounts:

| Host path | Mount path in container | Purpose |
|-----------|------------------------|---------|
| `/var/run/pv-snapshotter/` | `/var/run/pv-snapshotter/` | gRPC socket |
| `/run/containerd/` | `/run/containerd/` | containerd client (directory mount keeps the path valid across containerd restarts) |
| `/var/lib/kubelet` | `/var/lib/kubelet` | make CSI mount paths visible |

**Startup ordering:** containerd connects to the proxy plugin at startup and does **not** automatically reconnect if the plugin is unavailable. Ensure pv-snapshotter starts before containerd:

- `systemd`: use `After=` / `Requires=` directives
- `DaemonSet upgrade`: cordon → drain workload Pods → restart pv-snapshotter + containerd → uncordon

**Failure semantics:** if `pv-snapshotter` is unavailable, Pods with `runtimeClassName: pv` will fail to start. They will **not** silently fall back to plain overlayfs — this is intentional.

---

## Image Pull Requirement

pv-snapshotter maintains its own overlay snapshotter instance with a dedicated `metadata.db` under `--overlay-snapshotter.root-path`. Image layers must be unpacked into this snapshotter before any Pod using `runtimeClassName: pv` can start. If a container image was only pulled under the default `overlayfs` snapshotter, `Prepare()` will fail with `missing parent snapshot`.

**Automatic unpack via CRI on-demand (no extra config):**

You do **not** need `runtime_platforms` or any manual step on a healthy cluster. When kubelet calls `CreateContainer` for a Pod whose `runtimeClassName` routes to pv-snapshotter, containerd's CRI layer unpacks the image's layers into the container's snapshotter (pv-snapshotter) on demand, right before the container is created. The first such container for a given image pays a one-time sequential-unpack cost into pv-snapshotter's own `metadata.db`; subsequent containers reuse it.

> Because pv-snapshotter does not advertise the `rebase` capability, this unpack is sequential. For very large images the unpack can be slow — ensure kubelet's `runtimeRequestTimeout` is generous enough (e.g. 5m) so a slow first unpack is not cancelled mid-extraction.

**Manual fallback (Kubernetes < 1.29 or initial bootstrap):**

```bash
# Pull and unpack a single image into pv-snapshotter
ctr --namespace=k8s.io images pull \
  --snapshotter pv-snapshotter \
  <image>:<tag>

# List images already unpacked under pv-snapshotter
ctr --namespace=k8s.io images ls \
  --snapshotter pv-snapshotter

# Verify the image layers exist in pv-snapshotter's snapshot store
ctr --namespace=k8s.io snapshots \
  --snapshotter pv-snapshotter ls
```

This is the same operational requirement shared by all containerd proxy snapshotters (stargz-snapshotter, nydus-snapshotter, soci-snapshotter). pv-snapshotter cannot share the default overlayfs `metadata.db` because BoltDB uses an exclusive file lock — only one process may open it for writing at a time.

**Reducing duplicate storage with `--share-overlayfs-lowers` (v0.1.6+):**

When the host's native overlayfs snapshotter already has the image layers (e.g. the image was pulled before pv-snapshotter was introduced), enable `--share-overlayfs-lowers=true` to skip re-unpacking. pv-snapshotter creates lightweight "reference snapshots" whose `fs/` directory is a symlink into the host overlayfs layer directory, then pins those layers with a containerd lease to prevent GC. See [Changelog](#changelog) for operational details and kernel validation requirements.

---

## Helm Chart

The Helm chart is available at `charts/pv-snapshotter/`.

```bash
helm upgrade --install pv-snapshotter charts/pv-snapshotter \
  --namespace pv-snapshotter-system --create-namespace \
  --set image=your-registry/pv-snapshotter:vX.Y \
  --set "containerdConfig.runtimeClasses={runc,nvidia}"
```

Key values:

| Value | Default | Description |
|-------|---------|-------------|
| `image` | — | pv-snapshotter image (required) |
| `containerdConfig.runtimeClasses` | `[]` | Base runtime handler names to extend (e.g. `runc`, `nvidia`) |
| `containerdConfig.suffix` | `-pv` | Suffix appended to each base name (`runc` → `runc-pv`) |
| `unixSocketPath` | `/var/run/pv-snapshotter/daemon.sock` | gRPC socket path |
| `annotationPrefix` | `pv-snapshotter.humble-mun.io` | Pod annotation prefix |
| `tolerations` | control-plane NoSchedule | Node tolerations |
| `webhook.enabled` | `true` | Enable webhook, RBAC, cert-manager Certificate, and MutatingWebhookConfiguration |
| `webhook.clusterIssuerName` | `selfsigned` | cert-manager ClusterIssuer for the webhook TLS certificate |
| `webhook.objectSelector` | `matchLabels: pv-snapshotter.humble-mun.io/inject: "true"` | Only pods matching this selector are mutated |
| `webhook.pvcNameTemplate` | `{{.OwnerName}}` | Go template → PVC name to look up (a pod may override per-pod via the `<annotationPrefix>/pvc-name-template` annotation) |
| `webhook.maxOwnerDepth` | `2` | Owner-reference traversal depth |
| `webhook.defaultRuntimeClass` | `runc` | Base RuntimeClass when pod has none |
| `webhook.runtimeClassSuffix` | `-pv` | Suffix appended to form the pv-backed RuntimeClass name |
| `webhook.boundTimeout` | `10s` | Max wait for PVC Bound before denying the pod |
| `webhook.stateMountPath` | `/.platform/state` | Mount path injected into the primary container (`spec.containers[0]`) |
| `webhook.annotationTemplates` | ZFS LocalPV defaults | Go template map for annotations (illustrative; see note below) |

The chart deploys:
- A **ConfigMap** (`daemon.yaml`) that configures the daemon via viper (loaded from `/etc/humble-mun/daemon.yaml`).
- A **DaemonSet** with two containers:
  - `config` (native sidecar, `restartPolicy: Always`): waits for the daemon's `/readyz` endpoint, patches `/etc/containerd/config.toml` idempotently (copying the base runtime's config and adding `snapshotter = "pv-snapshotter"`), restarts containerd via a cgo nsenter preamble if needed, then blocks until Pod termination.
  - `daemon`: the pv-snapshotter gRPC proxy snapshotter and webhook server (when enabled).
- A **RuntimeClass** per entry in `containerdConfig.runtimeClasses`, named `<base><suffix>`.
- A **ServiceAccount** (`automountServiceAccountToken` follows `webhook.enabled`).
- When `webhook.enabled=true`: a **ClusterRole** + **ClusterRoleBinding** (PVC/PV and workload controller read access), a cert-manager **Certificate**, a ClusterIP **Service** (port 9443), and a **MutatingWebhookConfiguration**.

All daemon flags can be overridden via `HM_`-prefixed environment variables or entries in `daemon.yaml`.

> **`webhook.annotationTemplates` note:** This field documents the default annotation template values. The Helm chart passes them as `--webhook-annotation-templates` CLI arguments on the daemon container rather than writing them to `daemon.yaml`, to preserve map key casing (viper's YAML parser lowercases all keys). To customise annotation templates, override `webhook.annotationTemplates` in your `values.yaml`.

---

## CLI Flags

All flags can also be set via environment variables (uppercase, `_`-separated, prefixed with `HM_`).

### Snapshotter flags

| Flag | Default | Description |
|------|---------|-------------|
| `--unix-socket-path` | `/var/run/pv-snapshotter/daemon.sock` | gRPC listener socket path |
| `--containerd-socket` | `/run/containerd/containerd.sock` | containerd client socket |
| `--annotation-prefix` | `pv-snapshotter.humble-mun.io` | DNS subdomain prefix for Pod annotations (RFC 1123, no reserved domains) |
| `--overlay-snapshotter.root-path` | `/var/lib/containerd/io.containerd.snapshotter.v1.pv-snapshotter` | Native overlay snapshotter root |
| `--overlay-snapshotter.upper-dir-label` | `false` | Stamp `containerd.io/snapshot/overlay.upperdir` label on snapshots |
| `--overlay-snapshotter.sync-remove` | `false` | Synchronous snapshot removal |
| `--overlay-snapshotter.slow-chown` | `false` | Slow chown for ID-mapped mounts |
| `--overlay-snapshotter.mount-options` | `[]` | Extra mount options passed to overlayfs (**never add `volatile`**) |

> ⚠️ **Never add `volatile`** to `--overlay-snapshotter.mount-options`. It causes `upperdir` data loss on unclean shutdown, directly contradicting the persistence semantics of this project.

### Webhook flags

| Flag | Default | Description |
|------|---------|-------------|
| `--webhook-enabled` | `true` | Enable the mutating admission webhook endpoint |
| `--webhook-pvc-name-template` | `{{.OwnerName}}` | Go template → PVC name to look up |
| `--webhook-pvc-selector-template` | `""` | Go template → label selector; fallback when name template is empty |
| `--webhook-max-owner-depth` | `2` | Owner-reference traversal depth (0 = pod name directly) |
| `--webhook-default-runtime-class` | `runc` | Base RuntimeClass when pod specifies none |
| `--webhook-runtime-class-suffix` | `-pv` | Suffix appended to the base name |
| `--webhook-bound-timeout` | `10s` | Max wait for PVC to reach Bound before denying the pod |
| `--webhook-state-mount-path` | `/.platform/state` | Mount path injected into the primary container (`spec.containers[0]`) |
| `--webhook-annotation-templates` | ZFS LocalPV defaults | `key=value` CSV map of annotation key → Go template value |

---

## End-to-End Validation

`demo.yaml` contains a Deployment and a PVC that exercise the full pipeline end-to-end. Apply it to a cluster with the Helm chart installed:

```bash
kubectl apply -f docs/demo.yaml
```

The Deployment's pods carry the opt-in label `pv-snapshotter.humble-mun.io/inject: "true"`. The webhook resolves the owner chain (pod → ReplicaSet → Deployment `demo`), looks up PVC `demo`, waits for it to bind, then injects the state volume, annotations, and `runtimeClassName: runc-pv`.

**Verify:**

```bash
# 1. PVC bound
kubectl get pvc demo

# 2. Webhook-injected fields on the pod
POD=$(kubectl get pod -l app=demo -o name | head -1)
kubectl get $POD -o yaml | grep -E 'runtimeClassName|pv-snapshotter|pv-snapshotter--state'

# 3. Confirm upperdir is on the PVC (on the node that hosts the pod)
NODE_POD_UID=$(kubectl get $POD -o jsonpath='{.metadata.uid}')
# ssh <node> "findmnt -t overlay | grep $NODE_POD_UID"
# upperdir= should point to /var/lib/kubelet/pods/<uid>/volumes/…/upper

# 4. Write a sentinel file anywhere in the container's filesystem
#    (the overlay upperdir is on the PVC, so this survives pod recreation)
kubectl exec $POD -c demo -- sh -c 'echo ok > /tmp/sentinel.txt'

# 5. Delete the pod — the ReplicaSet recreates it against the same PVC
kubectl delete $POD
kubectl wait --for=condition=Ready pod -l app=demo --timeout=60s

# 6. Verify the file survived
kubectl exec $(kubectl get pod -l app=demo -o name | head -1) -c demo -- \
  cat /tmp/sentinel.txt
# expected: ok
```

**Teardown:**

```bash
kubectl delete -f docs/demo.yaml
```

---

## Observability

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

```bash
# On the node — find the container's overlay mount
findmnt -t overlay
# Verify upperdir= points to the provided path, not /var/lib/containerd/io.containerd.snapshotter.v1.pv-snapshotter/snapshots/...
```

### Log verbosity

```bash
# Pod lifecycle events (method calls, resolver steps, redirect routing)
--v=4

# Also dump raw sandbox metadata JSON
--v=5

# Correlate all logs for a specific container
grep 'key="k8s.io/31/4856f54d' /var/log/pv-snapshotter.log
```

### Confirm state persistence

```bash
# 1. Write a file inside the container
kubectl exec -it <pod> -- sh -c 'echo hello > /root/test.txt'

# 2. Delete the pod (its writable layer would normally be lost)
kubectl delete pod <pod>

# 3. Recreate a pod that references the same backing path
kubectl apply -f pod.yaml

# 4. Verify the file is still there
kubectl exec -it <pod> -- cat /root/test.txt
# hello
```

---

## Operational Notes

### Data Lifecycle Is the Caller's Responsibility

`Remove()` only cleans up the native overlay snapshot directories under the snapshotter root. The snapshotter never deletes the contents of the caller-provided path. If you need stop-without-loss versus destroy-and-reclaim semantics, implement that distinction in the component that owns the backing storage; the snapshotter does not decide it.

### upper/ and work/ Must Share a Filesystem

The snapshotter creates `upper/` and `work/` under the provided path. overlayfs requires both to be on the same filesystem — ensure the provided path is a single mounted volume.

### Storage Resize

If the backing volume is a CSI PVC, resize follows the standard CSI expansion path:

```bash
kubectl patch pvc <name> -p '{"spec":{"resources":{"requests":{"storage":"20Gi"}}}}'
```

The CSI driver resizes the block device; `resize2fs`/`xfs_growfs` runs online. No container restart is required.

### Keep the Backing Path Out of Reach

The provided path is the raw overlay upper directory. If you expose it inside the container (for example, by mounting the PVC at a dedicated path), prevent workloads from writing to it directly — enforce with AppArmor / SELinux if needed.

### `nerdctl commit`

A container backed by this snapshotter can be committed like any other:

```bash
nerdctl --namespace=k8s.io --snapshotter=pv-snapshotter commit <container> <image>
```

### Node Restart Recovery

After pv-snapshotter restarts, existing running containers still hold their overlay mounts (runc holds them). Re-created Pods that reference the same backing path correctly re-attach on the next `Mounts()` call.

---

## Upgrade Notes

### Migrating `rootPath` when upgrading from v0.1.4 or earlier

Starting with **v0.1.5**, the default snapshotter root path moved from
`/var/lib/containerd` to
`/var/lib/containerd/io.containerd.snapshotter.v1.pv-snapshotter`
(Helm value `overlaySnapshotter.rootPath` and the Go fallback default).

> **No migration needed if you keep the old path.** The new default only affects
> the *default* value. If you explicitly set `overlaySnapshotter.rootPath:
> /var/lib/containerd` (the v0.1.4 value), upgrading to v0.1.5 is a transparent
> drop-in — pv-snapshotter keeps using the existing `snapshots/` and `metadata.db`,
> and none of the steps below apply. The migration below is only required if you
> *also* choose to adopt the new default path.

This change is **not transparent on a node that already ran v0.1.4 or earlier**.
pv-snapshotter keeps its own `snapshots/` directory and `metadata.db` directly
under `rootPath`. If you simply change `rootPath` and restart:

- The new `metadata.db` starts empty and has no record of the existing snapshots.
- Image layers previously unpacked into pv-snapshotter are stranded under the old
  path; `Prepare()` reports `missing parent snapshot` for affected images.
- Running containers keep working (runc holds their overlay mounts), but newly
  created Pods using `runtimeClassName: pv` may fail until the affected images are
  re-unpacked.

**Recommended migration (fresh start on the new path):**

1. Cordon and drain the node so no `runtimeClassName: pv` Pods are scheduled.
2. Stop pv-snapshotter, then containerd.
3. Either move the old data into the new location, or start clean on the new path
   and let containerd re-unpack images on demand at `CreateContainer` time
   (RuntimeClass routing handles this automatically; no manual `ctr pull` is needed
   on Kubernetes 1.29+).
4. Restart containerd and pv-snapshotter; uncordon the node.

**If your node carries leftover stale metadata from the v0.1.4 disk-pressure
incident** (orphaned `pv-snapshotter` BoltDB buckets, or image records whose
backing blobs were GC'd), use the dedicated recovery tooling on the
[`docs/v0.1.4-recovery-tooling`](https://github.com/humble-mun/pv-snapshotter/tree/docs/v0.1.4-recovery-tooling)
branch:

- `docs/fix-meta/` — offline BoltDB tool that drops stale `pv-snapshotter`
  snapshot buckets from containerd's main `metadata.db` (SHA-256-verified backup,
  dry-run by default, `--apply` to commit).
- `docs/prune-images/` — platform-scoped tool that deletes image **records** whose
  this-node-platform config/layer blobs are actually missing, forcing kubelet to
  re-pull (fixes the "image looks present but cannot unpack" deadlock).
- `docs/recover-v0.1.4/` — a self-contained recovery DaemonSet that wires both
  tools together per node, plus a cleanup DaemonSet for removing the per-node
  recovery artifacts afterward. See that branch's `docs/recover-v0.1.4/README.md`
  for the full runbook.

---

## Changelog

### v0.1.8 — Shared annotation package + per-pod PVC override (CURRENT RELEASE)

- **Shared `pkg/annotation` package.** The `--annotation-prefix` logic (flag registration, RFC 1123 validation, prefix resolution, key building) was extracted into a constraint-free package shared by the snapshotter resolver and the mutating webhook. The flag is now registered by `annotation.RegisterFlags` (wired from `main.go`); the validation helper and reserved-domain constants are package-private.
- **Per-pod PVC name override.** A pod may set the `<annotation-prefix>/pvc-name-template` annotation (fixed suffix, configurable prefix) to override the global `--webhook-pvc-name-template` / `--webhook-pvc-selector-template`. The value is a Go template (same variables as the name template) or a literal PVC name; a rendered-empty value is rejected. This lets a pod bind a PVC whose lifecycle is independent of the pod/owner name.
- **Helm: leak-free map overrides.** `webhook.annotationTemplates` and `webhook.objectSelector` now default to `{}` in `values.yaml`. Helm deep-merges map values key-by-key and never drops a chart-default key, so non-empty map defaults leaked into user `-f` overrides. The canonical defaults now live where merge cannot reach: `annotationTemplates` falls back to the binary's compiled-in default (the DaemonSet only passes `--webhook-annotation-templates` when the map is non-empty), and the `objectSelector` opt-in label (`pv-snapshotter.humble-mun.io/inject: "true"`) moved into the `webhook.yaml` template `else` branch. A user-supplied value now fully **replaces** the default instead of being key-merged with it.
- **chassis v0.1.10 (config-loading fix).** Bumped `github.com/humble-mun/chassis` v0.1.7 → v0.1.10, which defers the global viper `SetConfigName`/`AddConfigPath` into the config-loader closure. Previously, registering the `config` subcommand overwrote the root command's config name on the shared global viper, so the daemon read the wrong file and silently fell back to compiled-in defaults (empty `root-path`, bind `0.0.0.0:8080`, default TLS path → crash). The daemon now reliably loads `/etc/humble-mun/daemon.yaml`.

### v0.1.7 — Orphan lease GC, scrape hook, string constants

**1. Orphan lease GC (`countOrphanLeases` / `gcOrphanLeases` in `dedup.go`)**

When `Remove()` fails to unpin a lease (e.g. pv-snapshotter restarted mid-flight), the
lease becomes "orphaned" — its owning active snapshot no longer exists in `localSn` but
the lease persists, keeping the overlayfs chainID pinned against GC. Two new methods on
`dedupManager` address this:

- `countOrphanLeases(ctx, ns)` — lists all leases labeled
  `pv-snapshotter.io/managed-by=pv-snapshotter`, checks each `owner-snapshot` label
  value against `localSn.Stat()`, and returns the count of orphans. Called by the scrape
  hook to refresh `pv_snapshotter_orphan_leases_total`.
- `gcOrphanLeases(ctx, ns)` — same traversal; deletes each orphan lease, calls
  `pinnedSnapshotsTotal.Dec()` per deletion, and returns the count deleted. Best-effort:
  logs errors and continues the sweep on failure.

**2. New Prometheus metric + `RegisterScrapeHook` implementation**

- `pv_snapshotter_orphan_leases_total{node_name}` GaugeVec added. Refreshed on every
  `/metrics` scrape.
- `RegisterScrapeHook(ctx context.Context)` was previously a TODO stub; it is now
  implemented: calls `countOrphanLeases` and updates the gauge. Wired in `main.go` via
  `metrics.RegisterScrapeHook(svc.RegisterScrapeHook)` (chassis v0.1.7 API).
- `POST /dedup/leases/gc` handler added: returns `{"deleted": N}` after calling
  `gcOrphanLeases`; returns 501 if dedup is not enabled.

**3. String constants refactor**

All bare `"k8s.io"` and `"kubernetes.io"` string literals in the snapshotter package
replaced with named constants defined in `resolver.go`:

| Constant | Value |
|----------|-------|
| `containerdNamespaceK8s` | `"k8s.io"` |
| `reservedAnnotationPrefixKubernetes` | `"kubernetes.io"` |
| `reservedAnnotationPrefixK8s` | `"k8s.io"` |

Affected files: `resolver.go` (constant definitions + `validateAnnotationPrefix`),
`service.go` (5 call sites), `dedup.go` (1 fallback namespace assignment). Import paths
and template string fragments (`kubernetes.io~csi`) are intentionally left unchanged.

---

### v0.1.6 — Dedup (`--share-overlayfs-lowers`) + sandbox upperdir fix

**1. Opportunistic dedup of read-only image layers (`--share-overlayfs-lowers`)**

pv-snapshotter previously re-unpacked image layers into its own `metadata.db` for
every image, duplicating the host's native overlayfs store. With
`--share-overlayfs-lowers=true` (opt-in, default false), pv-snapshotter reuses the
host overlayfs layers opportunistically:

- When `Stat(chainID)` returns not-found locally but the chainID exists in the host
  overlayfs snapshotter, a "reference snapshot" is created on demand: `Prepare` is
  called for the chainID, the new snapshot's `fs/` directory is replaced with a
  **symlink** pointing at the overlayfs layer's real physical directory
  (`/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/<N>/fs`),
  and the snapshot is committed. The kernel accepts this symlink as a lowerdir entry
  (verified on Linux 6.8).
- A containerd **lease** (no expiration) is created to pin the overlayfs chainID
  and its entire parent chain against GC. The lease carries two labels:
  `pv-snapshotter.io/managed-by=pv-snapshotter` and
  `pv-snapshotter.io/owner-snapshot=<activeSnapshotKey>`. On `Remove()`, the lease
  is deleted by querying the `owner-snapshot` label.
- If unpin fails, `pv_snapshotter_unpin_failures_total{node_name}` is incremented.
  Alert on `rate(pv_snapshotter_unpin_failures_total[5m]) > 0`. Use
  `DELETE /dedup/leases/:leaseID` for manual recovery.
- **Operational API**: `GET /dedup/leases` lists all managed leases (JSON);
  `DELETE /dedup/leases/:leaseID` removes one;
  `POST /dedup/leases/gc` triggers an orphan lease GC sweep (returns `{"deleted": N}`,
  see v0.1.7).
- **Prometheus metrics**: `pv_snapshotter_pinned_snapshots_total{node_name}` gauge;
  `pv_snapshotter_unpin_failures_total{node_name}` counter;
  `pv_snapshotter_orphan_leases_total{node_name}` gauge refreshed on scrape (see v0.1.7).
- **⚠️ Kernel re-validation required on upgrade**: the symlink-as-lowerdir behavior
  is a kernel implementation detail, not a documented API. Re-run P0-1 through P0-4
  on each new kernel version before enabling in production.

Enable via Helm: `--set overlaySnapshotter.shareOverlayfsLowers=true`

**2. Sandbox (pause) container upperdir redirection suppressed**

The resolver now skips upperdir redirection for sandbox (`pause`) containers.
Previously both the pause container and the workload container received the same
`upperdirPath`, causing two overlay mounts to share the same `workdir` — which the
kernel rejects. The pause container writes no business data; skipping it is safe and
correct. Only workload containers now get the PV-backed upperdir.

### v0.1.5 — Conventional snapshotter root path + upgrade guidance

- **Default `rootPath` moved** to `/var/lib/containerd/io.containerd.snapshotter.v1.pv-snapshotter`,
  following the `io.containerd.snapshotter.v1.<name>` convention. pv-snapshotter's
  own `snapshots/` and `metadata.db` now live in a self-contained subtree instead
  of loose at the root of containerd's data dir. Applies to the Helm chart default
  (`overlaySnapshotter.rootPath`) and the Go fallback default.
- **Fresh-install only.** Relocating `rootPath` on a node that already has
  pv-snapshotter snapshots strands the prior snapshots and `metadata.db`. See
  [Upgrade Notes](#upgrade-notes) for the migration path when upgrading from
  v0.1.4 or earlier.

### v0.1.4 — Fix GC disk accumulation (forward the `Cleanup()` gRPC call)

**Root cause.** With `syncRemove=false` (the default), `Remove()` only deletes the
BoltDB metadata record; the actual `os.RemoveAll` is deferred to a later `Cleanup()`
call.  `snapshotservice.FromSnapshotter` dispatches the gRPC Cleanup RPC via a
`snapshots.Cleaner` type assertion on the wrapped snapshotter.  Because the wrapper
struct embedded `snapshots.Snapshotter` as an interface (not the concrete
`*overlay.snapshotter`), the assertion always failed and returned `ErrNotImplemented`
— so every deleted container left a permanent orphaned directory on disk.  Nodes
running v0.1.3 accumulate these directories without bound and eventually hit disk
pressure.

**Fix.** Introduced `cleanerSnapshotter`, a thin wrapper around `snapshotter` that
holds a pre-checked `snapshots.Cleaner` reference obtained once at startup in
`RegisterGRPCService`.  `cleanerSnapshotter.Cleanup()` delegates directly to the
inner cleaner with no per-call type assertion.  When the underlying snapshotter does
not implement `snapshots.Cleaner`, the plain `snapshotter` is used instead (no
`Cleanup` exposed).

**Do not run v0.1.3 in production.**  Upgrade to v0.1.4 or later.

---

## Roadmap

pv-snapshotter is **production ready**. Core snapshotter, Helm chart, containerd config automation, and mutating admission webhook are all implemented and verified end-to-end. The items below are optional hardening and future enhancements.

### Optional Hardening

- [ ] Configurable cleanup behavior on `Remove()` (forget binding vs. reclaim backing storage)
- [ ] Node restart recovery — end-to-end verification that re-created pods re-attach correctly
- [ ] Storage expansion end-to-end testing
- ~~GC coordination: overlay metadata.db cleanup vs. backing storage lifecycle~~ — **Fixed in v0.1.4**

### Future

- [ ] Support for Ceph RBD globalmount staging path (automatic path detection via `sha256(volumeHandle)`)
- [ ] Multi-arch image builds (arm64)

---

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE) for details.
