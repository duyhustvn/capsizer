# ==============================================================================
# Capsizer Root Makefile
# Hỗ trợ biên dịch Go binaries, kiểm thử, đóng gói .deb và build/push Docker images
# ==============================================================================

VERSION            ?= 1.0.0
ARCH               ?= amd64
CONTAINER_REGISTRY ?= docker.io
DOCKER_USER        ?= duyle95

# Tên và tag cho 2 Docker images (Runner chính & Profiling Sidecar)
CAPSIZER_IMG       ?= capsizer
SIDECAR_IMG        ?= profiling-sidecar
IMG_TAG            ?= v$(VERSION)
BASE_IMAGE         ?= ubuntu:22.04

PROXY              ?= $(http_proxy)
NO_PROXY           ?= $(no_proxy)

FULL_CAPSIZER_IMG  = $(CONTAINER_REGISTRY)/$(DOCKER_USER)/$(CAPSIZER_IMG):$(IMG_TAG)
FULL_SIDECAR_IMG   = $(CONTAINER_REGISTRY)/$(DOCKER_USER)/$(SIDECAR_IMG):$(IMG_TAG)

GO_LDFLAGS         = -s -w

.PHONY: help all build build-bin test clean deb \
        docker-build docker-push sidecar-build sidecar-push docker-all push-all

help:
	@echo "================================================================================"
	@echo "                   CAPSIZER BUILD & DOCKER AUTOMATION"
	@echo "================================================================================"
	@echo "Sử dụng:"
	@echo "  make build          - Biên dịch tĩnh toàn bộ Go binaries vào ./bin/"
	@echo "  make test           - Chạy toàn bộ bộ kiểm thử (go test ./...)"
	@echo "  make deb            - Đóng gói bộ cài đặt Debian/Ubuntu (.deb) vào ./dist/"
	@echo "  make docker-build   - Build Docker image chính (capsizer runner/ramp/report)"
	@echo "  make docker-push    - Push Docker image chính lên registry"
	@echo "  make sidecar-build  - Build Docker image profiling-sidecar (probe, py-spy, perf, ss)"
	@echo "  make sidecar-push   - Push Docker image profiling-sidecar lên registry"
	@echo "  make docker-all     - Build đồng thời cả 2 Docker images (capsizer + sidecar)"
	@echo "  make push-all       - Push đồng thời cả 2 Docker images lên registry"
	@echo "  make clean          - Dọn dẹp các file binary tạm"
	@echo ""
	@echo "Biến tùy chỉnh:"
	@echo "  VERSION             (mặc định: $(VERSION))"
	@echo "  IMG_TAG             (mặc định: $(IMG_TAG))"
	@echo "  CONTAINER_REGISTRY  (mặc định: $(CONTAINER_REGISTRY))"
	@echo "  DOCKER_USER         (mặc định: $(DOCKER_USER))"
	@echo "  BASE_IMAGE          (mặc định: $(BASE_IMAGE))"
	@echo "  PROXY               (mặc định: $(PROXY))"
	@echo "================================================================================"

all: test build

# ------------------------------------------------------------------------------
# 1. Biên dịch Go Binaries (Static Linux Binaries)
# ------------------------------------------------------------------------------
build: build-bin
build-bin:
	@echo "==> Đang biên dịch tĩnh các Go binaries vào ./bin/..."
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="$(GO_LDFLAGS)" -o bin/capsizer ./cmd/capsizer
	CGO_ENABLED=0 go build -ldflags="$(GO_LDFLAGS)" -o bin/capsizer-ramp ./cmd/ramp
	CGO_ENABLED=0 go build -ldflags="$(GO_LDFLAGS)" -o bin/probe ./cmd/probe
	CGO_ENABLED=0 go build -ldflags="$(GO_LDFLAGS)" -o bin/capsizer-report ./cmd/report
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -ldflags="$(GO_LDFLAGS)" -o bin/probe-linux-amd64 ./cmd/probe
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -ldflags="$(GO_LDFLAGS)" -o bin/capsizer-ramp-linux-amd64 ./cmd/ramp
	@echo "    ✓ Hoàn tất biên dịch ./bin/capsizer, ./bin/capsizer-ramp, ./bin/probe, ./bin/capsizer-report"

# ------------------------------------------------------------------------------
# 2. Kiểm thử (Unit Tests)
# ------------------------------------------------------------------------------
test:
	go test -v ./...

# ------------------------------------------------------------------------------
# 3. Đóng gói Debian/Ubuntu (.deb)
# ------------------------------------------------------------------------------
deb:
	./packaging/build_deb.sh $(VERSION) $(ARCH)

# ------------------------------------------------------------------------------
# 4. Docker Image Chính (Capsizer Runner / Ramp / Probe / Report)
# ------------------------------------------------------------------------------
docker-build:
	@echo "==> Đang build Docker image $(CAPSIZER_IMG):$(IMG_TAG)..."
	docker build \
		--network host \
		--build-arg http_proxy=$(PROXY) \
		--build-arg https_proxy=$(PROXY) \
		--build-arg no_proxy=$(NO_PROXY) \
		--build-arg HTTP_PROXY=$(PROXY) \
		--build-arg HTTPS_PROXY=$(PROXY) \
		--build-arg NO_PROXY=$(NO_PROXY) \
		-t $(CAPSIZER_IMG):$(IMG_TAG) \
		-t $(CAPSIZER_IMG):latest \
		-t $(FULL_CAPSIZER_IMG) \
		-t $(CONTAINER_REGISTRY)/$(DOCKER_USER)/$(CAPSIZER_IMG):latest \
		-f Dockerfile .

docker-push:
	docker push $(FULL_CAPSIZER_IMG)
	docker push $(CONTAINER_REGISTRY)/$(DOCKER_USER)/$(CAPSIZER_IMG):latest

# ------------------------------------------------------------------------------
# 5. Docker Image Profiling Sidecar (probe, py-spy, perf, analyze-connections)
# ------------------------------------------------------------------------------
sidecar-build:
	@echo "==> Đang build Docker image $(SIDECAR_IMG):$(IMG_TAG)..."
	docker build \
		--network host \
		--build-arg BASE_IMAGE=$(BASE_IMAGE) \
		--build-arg http_proxy=$(PROXY) \
		--build-arg https_proxy=$(PROXY) \
		--build-arg no_proxy=$(NO_PROXY) \
		--build-arg HTTP_PROXY=$(PROXY) \
		--build-arg HTTPS_PROXY=$(PROXY) \
		--build-arg NO_PROXY=$(NO_PROXY) \
		-t $(SIDECAR_IMG):$(IMG_TAG) \
		-t $(SIDECAR_IMG):latest \
		-t $(FULL_SIDECAR_IMG) \
		-t $(CONTAINER_REGISTRY)/$(DOCKER_USER)/$(SIDECAR_IMG):latest \
		-f profiling-sidecar/Dockerfile .

sidecar-push:
	docker push $(FULL_SIDECAR_IMG)
	docker push $(CONTAINER_REGISTRY)/$(DOCKER_USER)/$(SIDECAR_IMG):latest

# ------------------------------------------------------------------------------
# 6. Build & Push Tất cả Docker Images
# ------------------------------------------------------------------------------
docker-all: docker-build sidecar-build

push-all: docker-push sidecar-push

clean:
	rm -rf bin/* build/deb/*
