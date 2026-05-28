# Tiny recovery image: ubuntu + fix-meta + recover.sh.
#
# Build context: docs/recover-v0.1.4/   (this directory)
# Build:
#   cp ../fix-meta/fix-meta-linux-amd64 ./fix-meta
#   docker build -t harbor.smoothcloud.com.cn/system/pv-snapshotter-recover:v1 .
#   docker push harbor.smoothcloud.com.cn/system/pv-snapshotter-recover:v1
FROM ubuntu:noble-20251013

COPY fix-meta   /usr/local/bin/fix-meta
COPY recover.sh /usr/local/bin/recover.sh

RUN chmod +x /usr/local/bin/fix-meta /usr/local/bin/recover.sh

ENTRYPOINT ["/usr/local/bin/recover.sh"]
