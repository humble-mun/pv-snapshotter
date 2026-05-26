.DEFAULT_GOAL := build
.PHONY: tag daemon build clean push

BASE_IMAGE ?= gcr.io/distroless/base-debian13:latest
NAME ?= pv-snapshotter
ARCH ?= amd64
VARIANT ?=
VERSION ?= v1.0
TIMESTAMP := $(shell date '+%Y%m%d%H%M')
TAG ?= $(VERSION)$(shell test "$(ARCH)" != amd64 && echo "-$(ARCH)" || true)$(shell test ! -z $(VARIANT) && echo "-$(VARIANT)" || true)-$(TIMESTAMP)
DEBUG ?= false
REPO ?= harbor-c0a811fc.nip.io/humble-mun/$(NAME)
DOCKERFILE ?= Dockerfile

ifeq "$(DEBUG)" "false"
LD_FLAGS ?= -w -s
else
GC_FLAGS ?= -gcflags \"all=-N -l\"
endif

tag:
	@echo "TAG=$(TAG)"

clean:
	docker images '$(REPO)*' --format='{{ .Repository }}:{{ .Tag }}' | xargs docker rmi

push:
	docker images --filter=reference="$(REPO)*" --format="{{ .Repository }}:{{ .Tag }}" | xargs -L1 docker push

daemon:
	docker --debug build --no-cache --platform linux/$(ARCH) \
	--build-arg BASE_IMAGE="$(BASE_IMAGE)" \
	--build-arg LD_FLAGS="$(LD_FLAGS)" \
	--build-arg GC_FLAGS="$(GC_FLAGS)" \
	--build-arg ARCH="$(ARCH)" \
	--build-arg VARIANT="$(VARIANT)" \
	-t $(REPO):$(TAG) -f $(DOCKERFILE) .

build: daemon
