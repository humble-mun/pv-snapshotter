# Tiny recovery image: ubuntu + fix-meta + prune-images + recover.sh + cleanup.sh.
#
# Build context: docs/recover-v0.1.4/   (this directory)
# Build:
#   cp ../fix-meta/fix-meta-linux-amd64           ./fix-meta
#   cp ../prune-images/prune-images-linux-amd64   ./prune-images
#   docker build -t harbor.smoothcloud.com.cn/system/pv-snapshotter-recover:v3 .
#   docker push harbor.smoothcloud.com.cn/system/pv-snapshotter-recover:v3
#
# Entrypoint is selected per-DaemonSet:
#   recover.sh (default)  — daemonset.yaml / daemonset-enhanced.yaml
#   cleanup.sh (override)  — cleanup-daemonset.yaml (command: ["/usr/local/bin/cleanup.sh"])
FROM ubuntu:noble-20251013

COPY fix-meta      /usr/local/bin/fix-meta
COPY prune-images  /usr/local/bin/prune-images
COPY recover.sh    /usr/local/bin/recover.sh
COPY cleanup.sh    /usr/local/bin/cleanup.sh

RUN chmod +x /usr/local/bin/fix-meta /usr/local/bin/prune-images /usr/local/bin/recover.sh /usr/local/bin/cleanup.sh

ENTRYPOINT ["/usr/local/bin/recover.sh"]
