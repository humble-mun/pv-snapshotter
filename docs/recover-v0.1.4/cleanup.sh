#!/usr/bin/env bash
# pv-snapshotter recovery CLEANUP script.
#
# Purpose: remove the per-node host-side state that recover.sh (the recovery
# DaemonSet) leaves behind, so a node can be re-experimented on from a clean
# slate.  This does NOT touch containerd, snapshots, content blobs or the
# metadata DB itself — it only clears the recovery tooling's own artifacts.
#
# What it removes on each labelled node:
#   - the transient systemd unit  pv-snapshotter-recover.service  (stop + reset-failed)
#   - marker      /var/lib/pv-snapshotter-recover.done
#   - log         /var/log/pv-snapshotter-recover.log
#   - staged      /var/tmp/pv-recover-fix-meta
#   - inner       /var/tmp/pv-recover-inner.sh
#   - precheck    /var/tmp/pv-recover-precheck.db
#
# Optional (CLEAN_BACKUPS=true): also delete the meta.db backups created during
# recovery (meta.db.recover-* and meta.db.fix-meta-backup-*).  These are kept by
# default because they are the rollback safety net; only purge them once the
# node is confirmed healthy.
#
# Lifecycle: runs as the entrypoint of a privileged DaemonSet container with the
# host root mounted at /host and hostPID enabled.  It performs the cleanup once
# then sleeps, so the DaemonSet pod stays Ready.  Safe to re-run (idempotent).

set -euo pipefail

readonly NS=(nsenter -t 1 -m -p -n -i -u --)
readonly UNIT=pv-snapshotter-recover
CLEAN_BACKUPS="${CLEAN_BACKUPS:-false}"

log() {
  echo "[$(date '+%Y-%m-%dT%H:%M:%S%z')] $*"
}

log "=== pv-snapshotter recovery cleanup starting on node ${HM_NODE_NAME:-unknown} ==="

# ── Step 1: tear down the transient systemd unit (host context) ───────────────
"${NS[@]}" systemctl stop          "${UNIT}.service" 2>/dev/null || true
"${NS[@]}" systemctl reset-failed  "${UNIT}.service" 2>/dev/null || true
log "systemd unit ${UNIT}.service stopped and reset (if it existed)"

# ── Step 2: remove operational artifacts + marker (enables a clean re-run) ─────
for f in \
  /host/var/lib/pv-snapshotter-recover.done \
  /host/var/log/pv-snapshotter-recover.log \
  /host/var/tmp/pv-recover-fix-meta \
  /host/var/tmp/pv-recover-prune-images \
  /host/var/tmp/pv-recover-inner.sh \
  /host/var/tmp/pv-recover-precheck.db ; do
  if [[ -e "${f}" ]]; then
    rm -f "${f}" && log "removed ${f#/host}"
  fi
done

# ── Step 3: optionally purge meta.db backups ──────────────────────────────────
readonly BOLT_DIR=/host/var/lib/containerd/io.containerd.metadata.v1.bolt
if [[ "${CLEAN_BACKUPS}" == "true" ]]; then
  shopt -s nullglob
  for b in "${BOLT_DIR}"/meta.db.recover-* "${BOLT_DIR}"/meta.db.fix-meta-backup-* ; do
    rm -f "${b}" && log "removed backup ${b#/host}"
  done
  shopt -u nullglob
else
  log "CLEAN_BACKUPS!=true → keeping meta.db backups (rollback safety net)"
fi

log "=== cleanup done on node ${HM_NODE_NAME:-unknown} ==="
exec sleep infinity
