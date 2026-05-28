#!/usr/bin/env bash
# pv-snapshotter v0.1.4 metadata recovery script.
#
# Purpose: drop the stale `v1/<ns>/snapshots/pv-snapshotter` bucket left by
# the v0.1.4 incident in containerd's main metadata, then trigger one explicit
# image.Unpack of the sandbox (pause) image so pv-snapshotter's own metadata.db
# is repopulated consistently with the main metadata.  All subsequent images
# auto-recover via CRI lazy-unpack at container creation time.
#
# Lifecycle: this script runs as the entrypoint of a privileged DaemonSet
# container.  It performs cheap, read-only checks itself; if a full recovery
# is required (which involves restarting containerd, which will kill this
# very container), the recovery is registered with the HOST systemd as a
# transient .service unit via `systemd-run`.  That unit survives the container
# death, completes the work, then writes a marker file.  The container is
# restarted by kubelet after containerd comes back, sees the marker, and
# sleeps.
#
# Idempotency: re-running on a healthy node is a no-op (marker check; or
# fresh dry-run + ctr pull validation if marker is missing).

set -euo pipefail

readonly MARKER_HOST=/var/lib/pv-snapshotter-recover.done
readonly MARKER=/host${MARKER_HOST}
readonly LOG_HOST=/var/log/pv-snapshotter-recover.log
readonly LOG=/host${LOG_HOST}
readonly NS=(nsenter -t 1 -m -p -n -i -u --)
readonly UNIT=pv-snapshotter-recover

log() {
  local msg="[$(date '+%Y-%m-%dT%H:%M:%S%z')] $*"
  echo "${msg}"
  echo "${msg}" >> "${LOG}" 2>/dev/null || true
}

die() {
  log "FATAL: $*"
  exit 1
}

mkdir -p /host/var/lib /host/var/log /host/var/tmp
touch "${LOG}" 2>/dev/null || true

# ── Phase 0: short-circuit on marker ──────────────────────────────────────────
if [[ -f "${MARKER}" ]]; then
  log "marker ${MARKER_HOST} present; recovery already done on this node — sleeping"
  exec sleep infinity
fi

log "=== pv-snapshotter v0.1.4 recovery starting on node ${HM_NODE_NAME:-unknown} ==="

# ── Phase 1: discover sandbox image from containerd config ────────────────────
SANDBOX_IMAGE=$(awk -F'"' '/^[[:space:]]*sandbox_image/{print $2; exit}' \
  /host/etc/containerd/config.toml)
[[ -n "${SANDBOX_IMAGE}" ]] || die "could not detect sandbox_image from /host/etc/containerd/config.toml"
log "sandbox_image = ${SANDBOX_IMAGE}"

# ── Phase 2: read-only state inspection (no containerd interruption) ──────────
# fix-meta opens BoltDB exclusively even read-only, so operate on a copy.
"${NS[@]}" cp /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db \
  /var/tmp/pv-recover-precheck.db
PRECHECK=$("${NS[@]}" /usr/local/bin/fix-meta /var/tmp/pv-recover-precheck.db 2>&1 || true)
"${NS[@]}" rm -f /var/tmp/pv-recover-precheck.db
STALE_COUNT=$(echo "${PRECHECK}" \
  | awk '/plan summary:.*entries total/ {for(i=1;i<=NF;i++) if ($i ~ /^[0-9]+$/) print $i}' \
  | tail -1)
STALE_COUNT=${STALE_COUNT:-0}
log "stale pv-snapshotter entries = ${STALE_COUNT}"

# ── Phase 3: if stale=0, try a one-shot ctr pull to validate consistency ──────
if [[ "${STALE_COUNT}" == "0" ]]; then
  if [[ -S /host/var/run/pv-snapshotter/daemon.sock ]]; then
    if "${NS[@]}" ctr -n k8s.io image pull -k --local \
         --snapshotter pv-snapshotter "${SANDBOX_IMAGE}" >/dev/null 2>&1; then
      log "node already healthy (no stale buckets, pv-snapshotter pull OK); marking done"
      echo "$(date '+%Y-%m-%dT%H:%M:%S%z') already-healthy" > "${MARKER}"
      log "=== recovery complete (no-op) ==="
      exec sleep infinity
    else
      log "stale=0 but ctr pull failed; proceeding with full recovery"
    fi
  else
    log "stale=0 but pv-snapshotter daemon socket missing; proceeding with full recovery"
  fi
fi

# ── Phase 4: stage fix-meta + inner script on host ────────────────────────────
log "staging fix-meta and inner recovery script on host ..."
cp /usr/local/bin/fix-meta /host/var/tmp/pv-recover-fix-meta
chmod +x /host/var/tmp/pv-recover-fix-meta

# The inner script runs in HOST context as a systemd transient unit. Variables
# referenced with ${VAR} are substituted at OUTER (this-script) time; variables
# that must defer to inner-script execution are escaped as \${VAR}.
cat > /host/var/tmp/pv-recover-inner.sh <<INNER_EOF
#!/usr/bin/env bash
set -euo pipefail

LOG=${LOG_HOST}
MARKER=${MARKER_HOST}
SANDBOX_IMAGE='${SANDBOX_IMAGE}'
META=/var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db

log() {
  echo "[\$(date '+%Y-%m-%dT%H:%M:%S%z')] \$*" >> "\${LOG}"
}

# Safety net: no matter how this script exits, ensure containerd + kubelet
# are running before we leave.  Without this, a failure between "stop
# containerd" and "start containerd" would leave the node unable to run
# any containers at all.
on_exit() {
  local rc=\$?
  log "exit trap fired (rc=\${rc}); ensuring containerd and kubelet are up"
  systemctl start containerd 2>/dev/null || log "WARN: failed to start containerd from trap"
  systemctl start kubelet    2>/dev/null || log "WARN: failed to start kubelet from trap"
  exit \${rc}
}
trap on_exit EXIT

log "=== inner recovery unit pv-snapshotter-recover.service starting ==="
log "sandbox_image = \${SANDBOX_IMAGE}"

TS=\$(date +%s)
log "stopping kubelet ..."
systemctl stop kubelet || log "WARN: systemctl stop kubelet returned non-zero"

log "stopping containerd ..."
systemctl stop containerd || log "WARN: systemctl stop containerd returned non-zero"

log "backing up meta.db to \${META}.recover-\${TS} ..."
cp "\${META}" "\${META}.recover-\${TS}"

log "running fix-meta --apply ..."
/var/tmp/pv-recover-fix-meta "\${META}" --apply >> "\${LOG}" 2>&1

log "starting containerd ..."
systemctl start containerd

log "waiting for /var/run/pv-snapshotter/daemon.sock (up to 120s) ..."
for i in \$(seq 1 120); do
  if [[ -S /var/run/pv-snapshotter/daemon.sock ]]; then
    log "daemon socket ready after \${i}s"
    break
  fi
  sleep 1
done
if [[ ! -S /var/run/pv-snapshotter/daemon.sock ]]; then
  log "FATAL: daemon socket never appeared after 120s"
  exit 1
fi

log "triggering image.Unpack via ctr pull --local --snapshotter pv-snapshotter \${SANDBOX_IMAGE} ..."
ctr -n k8s.io image pull -k --local \
  --snapshotter pv-snapshotter "\${SANDBOX_IMAGE}" >> "\${LOG}" 2>&1

log "starting kubelet ..."
systemctl start kubelet

log "writing marker \${MARKER} ..."
echo "\$(date '+%Y-%m-%dT%H:%M:%S%z') recovered" > "\${MARKER}"

log "=== inner recovery done ==="
INNER_EOF
chmod +x /host/var/tmp/pv-recover-inner.sh

# ── Phase 5: launch host-side recovery as a transient systemd unit ────────────
# `--collect` cleans up the unit after exit (regardless of success/failure).
# systemd-run returns immediately after the unit is started; the DaemonSet
# container is then free to sleep.  When the inner unit issues `systemctl stop
# containerd`, this container is killed, but the inner unit (owned by host
# systemd) continues uninterrupted.

# If a previous attempt left a stale unit behind, reset it first.
"${NS[@]}" systemctl reset-failed "${UNIT}.service" 2>/dev/null || true
"${NS[@]}" systemctl stop          "${UNIT}.service" 2>/dev/null || true

log "launching transient systemd unit ${UNIT}.service on host ..."
"${NS[@]}" systemd-run \
  --unit="${UNIT}" \
  --description="pv-snapshotter v0.1.4 metadata recovery" \
  --collect \
  --no-block \
  /var/tmp/pv-recover-inner.sh

log "unit launched; this container will sleep and likely be restarted by kubelet"
log "host-side progress: tail -f ${LOG_HOST}"
log "host-side unit status: systemctl status ${UNIT}.service (on the node)"

# The unit will trigger `systemctl stop containerd` within seconds, which kills
# THIS container.  kubelet will restart the pod after containerd is back up;
# the restarted entrypoint sees the marker file and short-circuits.
exec sleep infinity
