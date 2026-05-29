# pv-snapshotter v0.1.4 incident recovery

One-shot recovery for nodes polluted by the v0.1.4 metadata-inconsistency
incident.  See `../v0.1.4-incident-report.md` for full background.

## What it does

For each polluted node, in order:

1. **Detect** the sandbox image (`pause`) from `/etc/containerd/config.toml`.
2. **Read-only health probe**: count stale `v1/<ns>/snapshots/pv-snapshotter`
   entries in containerd's main metadata BoltDB and probe `ctr pull --local
   --snapshotter pv-snapshotter <pause>`.  If stale=0 AND probe succeeds →
   mark done and exit without touching containerd.
3. **Full recovery** (only if needed):
   1. `systemctl stop kubelet`
   2. `systemctl stop containerd`
   3. backup `meta.db` to `meta.db.recover-<timestamp>`
   4. `fix-meta --apply` (drops stale pv-snapshotter buckets in one BoltDB
      transaction)
   5. `systemctl start containerd`
   6. wait for `/var/run/pv-snapshotter/daemon.sock`
   7. `ctr -n k8s.io image pull -k --local --snapshotter pv-snapshotter
      <pause>` (this is the call that triggers `image.Unpack` and consistently
      populates pv-snapshotter's own metadata.db)
   8. `prune-images --apply` — delete image **records** whose backing blobs for
      **this node's platform** (e.g. amd64) are actually missing from the
      content store, forcing kubelet to re-pull them. Fixes the v0.1.4 §5.4
      缺口2 deadlock (manifest record present but layer blobs GC'd → kubelet
      skips PullImage → on-demand unpack fails). Platform-scoped per doc §6 —
      it does **not** use `ctr images check` incomplete (the §5.6 trap that
      misjudges healthy multi-arch images). Non-fatal: a non-zero exit is
      logged and recovery continues.
   9. `systemctl start kubelet`
   10. write marker file `/var/lib/pv-snapshotter-recover.done`

The full recovery runs as a **transient systemd unit on the host**
(`pv-snapshotter-recover.service`), launched via `systemd-run`.  The unit
survives the containerd restart that kills the DaemonSet pod.

## Properties

- **Idempotent**: marker file + state probes; re-running on a healthy node
  is a cheap no-op.
- **Serialised**: `maxUnavailable: 1` ensures one node at a time.
- **Bounded blast radius**: containerd is down ~5–10 seconds per node.
  Existing containers continue running (overlay mounts are kernel-held).
  Only in-flight gRPC calls are interrupted.
- **Self-evident logging**: progress goes to both the DaemonSet pod's
  stdout (visible via `kubectl logs`) and to `/var/log/pv-snapshotter-recover.log`
  on the host.

## Build & deploy

### Step 1 — build & push the recovery image

```bash
cd docs/recover-v0.1.4/

# Stage binaries into the build context.
cp ../fix-meta/fix-meta-linux-amd64 ./fix-meta
cp ../prune-images/prune-images-linux-amd64 ./prune-images

# Build & push.
docker build -t harbor.smoothcloud.com.cn/system/pv-snapshotter-recover:v3 .
docker push   harbor.smoothcloud.com.cn/system/pv-snapshotter-recover:v3
```

### Step 2 — deploy the DaemonSet (no Pods yet — no nodes labelled)

```bash
kubectl apply -f daemonset.yaml
# The DaemonSet exists but matches 0 nodes because no node has the
# pv-snapshotter-recover/enabled=true label yet.
kubectl -n kube-system get ds pv-snapshotter-recover
# DESIRED=0  CURRENT=0  READY=0 ...
```

### Step 3 — wave rollout via node labelling

`maxUnavailable: 1` only governs rolling updates (spec changes), not initial
Pod scheduling.  To avoid every node restarting containerd at the same time,
opt nodes in by labelling them in waves.

```bash
# --- Wave 1: pilot on a single polluted node ---
# Pick one node that currently reproduces the bug.
PILOT=10.254.17.42   # ← change to a real polluted node name
kubectl label node "$PILOT" pv-snapshotter-recover/enabled=true

# Watch progress (a few options):
#   a) DaemonSet pod stdout (cheap from kubectl):
kubectl -n kube-system logs -l app=pv-snapshotter-recover --prefix --tail=200
#   b) host-side service status (most accurate):
ssh "$PILOT" 'journalctl -u pv-snapshotter-recover --no-pager -n 100'
#   c) host-side log file:
ssh "$PILOT" 'tail -n 100 /var/log/pv-snapshotter-recover.log'

# Verify pilot success:
ssh "$PILOT" 'cat /var/lib/pv-snapshotter-recover.done'
# Should print: "<timestamp> recovered" (or "already-healthy" if no-op).

# Confirm a previously-failing Pod can now schedule on the pilot:
kubectl -n <ns> delete pod <failing-pod>
kubectl -n <ns> get pod <failing-pod> -o wide -w

# --- Wave 2: small batch (5–10 nodes) ---
NODES=(10.254.17.43 10.254.17.44 10.254.17.45 ...)
for n in "${NODES[@]}"; do
  kubectl label node "$n" pv-snapshotter-recover/enabled=true
done
# Wait for all markers before proceeding:
for n in "${NODES[@]}"; do
  echo -n "$n: "
  ssh "$n" 'cat /var/lib/pv-snapshotter-recover.done 2>/dev/null || echo PENDING'
done

# --- Wave 3: remainder ---
# Label all remaining nodes.  At this point you have confidence; the brief
# parallel containerd restarts are acceptable.
kubectl get node -o name | xargs -I{} kubectl label {} \
  pv-snapshotter-recover/enabled=true --overwrite
```

## Verifying completion

A node is recovered when **all three** are true:

- `/var/lib/pv-snapshotter-recover.done` exists on the host
- `fix-meta` dry-run on a copy of the main `meta.db` shows 0 entries to drop
  *(or shows 1 entry that corresponds to a successful unpack — i.e. the
  FRESH pause bucket)*
- `/var/lib/containerd/metadata.db` has been written by pv-snapshotter
  (mtime is after recovery completion)

Cluster-wide:

```bash
# Count nodes with the marker file via a quick DaemonSet pod exec
for pod in $(kubectl -n kube-system get pod -l app=pv-snapshotter-recover -o name); do
  node=$(kubectl -n kube-system get "$pod" -o jsonpath='{.spec.nodeName}')
  status=$(kubectl -n kube-system exec "$pod" -- \
    bash -c '[ -f /host/var/lib/pv-snapshotter-recover.done ] && cat /host/var/lib/pv-snapshotter-recover.done || echo MISSING')
  echo "${node}: ${status}"
done
```

## Cleanup

Once every node reports `recovered` (or `already-healthy`):

```bash
kubectl delete -f daemonset.yaml

# Remove the opt-in label from all nodes (optional but tidy):
kubectl get node -o name | xargs -I{} kubectl label {} \
  pv-snapshotter-recover/enabled-

# Optional: also remove host-side artifacts on each node
#   /var/tmp/pv-recover-fix-meta
#   /var/tmp/pv-recover-inner.sh
#   /var/log/pv-snapshotter-recover.log
#   /var/lib/pv-snapshotter-recover.done
# These are tiny; leaving them is harmless.
```

## Manual rollback (if something goes wrong on a node)

Each recovery backs up `meta.db` to `meta.db.recover-<timestamp>` before
calling `fix-meta`.  To restore:

```bash
systemctl stop containerd
ls -la /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db*
cp /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db.recover-<TS> \
   /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db
systemctl start containerd
rm /var/lib/pv-snapshotter-recover.done   # to allow re-attempt
```

## Files

| File | Purpose |
|------|---------|
| `Dockerfile` | Builds the recovery image (`:v2`): ubuntu + fix-meta + recover.sh + cleanup.sh |
| `recover.sh` | DaemonSet entrypoint; orchestrates the recovery (fix-meta drop + sandbox re-unpack) |
| `daemonset.yaml` | Privileged DaemonSet that runs `recover.sh` on each node |
| `cleanup.sh` | Removes per-node host-side recovery artifacts (unit, marker, tmp files; optional backups) |
| `cleanup-daemonset.yaml` | Runs `cleanup.sh`; scoped via `pv-snapshotter-recover-cleanup/enabled=true` |
| `README.md` | This file |

## Cleanup between runs (v2)

The `:v3` image bundles `cleanup.sh` so a node's host-side recovery state
(transient systemd unit, marker, tmp files; optional meta.db backups) can be
reset — useful for re-running on a node that already has the `recovered`
marker.

Rebuild the image with both scripts:

```bash
cd docs/recover-v0.1.4/
cp ../fix-meta/fix-meta-linux-amd64 ./fix-meta
cp ../prune-images/prune-images-linux-amd64 ./prune-images
docker build -t harbor.smoothcloud.com.cn/system/pv-snapshotter-recover:v3 .
docker push   harbor.smoothcloud.com.cn/system/pv-snapshotter-recover:v3
```

Reset a node:

```bash
kubectl apply -f cleanup-daemonset.yaml
kubectl label node <NODE> pv-snapshotter-recover-cleanup/enabled=true
# (set CLEAN_BACKUPS=true in cleanup-daemonset.yaml to also purge meta.db backups)
```

> NOTE on automated image-record repair: an earlier idea was to auto-`ctr images
> rm` every image `ctr images check` reports as "incomplete", to break the
> half-populated-record deadlock.  Empirically this is **unsafe**: `ctr images
> check` reports "incomplete" for healthy, runnable images too (multi-arch index
> siblings, attestation/SBOM blobs, partially-GC'd-but-amd64-complete images) —
> the recovery image itself shows `incomplete (5/6)` while running fine.  Any
> automated repair must instead resolve the node-platform manifest and verify
> only its own config+layers, which `ctr images check` cannot express; do it
> with a platform-scoped tool, never a blanket "incomplete" sweep.
