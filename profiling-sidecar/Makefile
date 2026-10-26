IMG_NAME ?= profiling-sidecar
IMG_TAG ?= v1.0.0

CONTAINER_REGISTRY ?= docker.io
DOCKER_USER ?= duyle95

BASE_IMAGE ?= ubuntu:22.04

PROXY ?= $(http_proxy)
NO_PROXY ?= $(no_proxy)

FULL_IMAGE = $(CONTAINER_REGISTRY)/$(DOCKER_USER)/$(IMG_NAME):$(IMG_TAG)

.PHONY: help build push all docker-build docker-push

help:
	@echo "Usage:"
	@echo "  make build       - Build docker image"
	@echo "  make push        - Push docker image to registry"
	@echo "  make all         - Build and push docker image"
	@echo ""
	@echo "Variables:"
	@echo "  IMG_NAME            (default: $(IMG_NAME))"
	@echo "  IMG_TAG             (default: $(IMG_TAG))"
	@echo "  CONTAINER_REGISTRY  (default: $(CONTAINER_REGISTRY))"
	@echo "  DOCKER_USER         (default: $(DOCKER_USER))"
	@echo "  BASE_IMAGE          (default: $(BASE_IMAGE))"
	@echo "  PROXY               (default: $(PROXY))"

build: docker-build
docker-build:
	docker build \
		--build-arg BASE_IMAGE=$(BASE_IMAGE) \
		--build-arg http_proxy=$(PROXY) \
		--build-arg https_proxy=$(PROXY) \
		--build-arg no_proxy=$(NO_PROXY) \
		--build-arg HTTP_PROXY=$(PROXY) \
		--build-arg HTTPS_PROXY=$(PROXY) \
		--build-arg NO_PROXY=$(NO_PROXY) \
		-t $(IMG_NAME):$(IMG_TAG) \
		-t $(FULL_IMAGE) \
		-f Dockerfile ..

push: docker-push
docker-push:
	docker push $(FULL_IMAGE)

all: build push
