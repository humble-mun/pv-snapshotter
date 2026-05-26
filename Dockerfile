ARG GO_VERSION=1.26.3-trixie
ARG BASE_IMAGE=gcr.io/distroless/base-debian13:latest

FROM golang:${GO_VERSION} AS builder

ARG BASE_PROJECT=github.com/humble-mun/chassis
ARG PROJECT=github.com/humble-mun/pv-snapshotter
ARG ARCH=amd64
ARG VERSION_PACKAGE=pkg/version
ARG NAME=daemon
ARG VARIANT
ARG GC_FLAGS
ARG LD_FLAGS="-w -s"

# CGO_ENABLED=1 is required for the cgo nsenter preamble (setns before Go
# runtime starts).  The resulting binary links against glibc only (no libgcc
# or libstdc++), so distroless/base (glibc + CA certs, no C++ runtime) is
# sufficient as the runtime image.
ENV CGO_ENABLED=1 GOOS=linux GOARCH=${ARCH}

WORKDIR /go/src/${PROJECT}
RUN --mount=type=bind,source=/,target=/go/src/${PROJECT} go build \
-v -mod=vendor ${GC_FLAGS} -ldflags \
"${LD_FLAGS} -X \"${BASE_PROJECT}/${VERSION_PACKAGE}.CommitID=`git rev-parse HEAD`\" \
-X \"${BASE_PROJECT}/${VERSION_PACKAGE}.BuiltAt=`date -u +'%Y-%m-%dT%H:%M:%SZ'`\" \
-X \"${BASE_PROJECT}/${VERSION_PACKAGE}.Name=${NAME}\" \
-X \"${BASE_PROJECT}/${VERSION_PACKAGE}.Architecture=${ARCH}\" \
-X \"${BASE_PROJECT}/${VERSION_PACKAGE}.Variant=${VARIANT}\" \
-X \"${BASE_PROJECT}/${VERSION_PACKAGE}.RecentCommits=`git log -n 20 --oneline | tee /dev/null `\"" \
-o /opt/humble-mun/${NAME}.elf ${PROJECT}/cmd/${NAME}
FROM ${BASE_IMAGE}
ARG NAME=daemon
ENV GIN_MODE=release
COPY --from=builder /opt/humble-mun/${NAME}.elf /usr/local/bin/${NAME}
VOLUME ["/etc/humble-mun"]
EXPOSE 8080
ENTRYPOINT ["daemon"]
