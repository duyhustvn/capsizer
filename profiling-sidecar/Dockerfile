ARG BASE_IMAGE=ubuntu:22.04

# ==========================================
# Stage 1: Go Builder (Biên dịch probe binary)
# ==========================================
FROM golang:alpine AS go-builder

WORKDIR /build
COPY go.mod ./
COPY pkg/ ./pkg/
COPY cmd/probe/ ./cmd/probe/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o probe ./cmd/probe

# ==========================================
# Stage 2: Tools Builder (Tải & chuẩn bị py-spy, FlameGraph)
# ==========================================
FROM ${BASE_IMAGE} AS builder

ARG http_proxy
ARG https_proxy
ARG no_proxy
ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        python3-pip \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Cài py-spy để lấy binary
RUN pip3 install --no-cache-dir py-spy==0.4.2

# Clone FlameGraph và dọn dẹp .git để tối ưu dung lượng
RUN git clone --depth 1 https://github.com/brendangregg/FlameGraph /opt/FlameGraph \
    && rm -rf /opt/FlameGraph/.git

# ==========================================
# Stage 3: Final Runtime Image
# ==========================================
FROM ${BASE_IMAGE}

ARG http_proxy
ARG https_proxy
ARG no_proxy
ARG DEBIAN_FRONTEND=noninteractive

# Chỉ cài các runtime packages cần thiết
RUN apt-get update && apt-get install -y --no-install-recommends \
        bsdextrautils \
        busybox-static \
        iproute2 \
        linux-tools-generic \
        linux-tools-common \
        perl \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Fix lỗi "perf not found for kernel x.x.x":
# Thay thế wrapper mặc định của Ubuntu bằng script tự động fallback sang binary perf có sẵn trong container
COPY perf-wrapper.sh /usr/local/bin/perf
RUN chmod +x /usr/local/bin/perf \
    && ln -sf /usr/local/bin/perf /usr/bin/perf

# Copy binaries và scripts từ các stage builder
COPY --from=builder /usr/local/bin/py-spy /usr/local/bin/py-spy
COPY --from=builder /opt/FlameGraph /opt/FlameGraph
COPY --from=go-builder /build/probe /usr/local/bin/probe

# Copy các helper scripts hữu ích (py-spy, perf flamegraph, analyze-connections)
COPY scripts/ /usr/local/bin/
RUN chmod +x /usr/local/bin/*.sh \
    && ln -sf /usr/local/bin/profile-pyspy.sh /usr/local/bin/profile-pyspy \
    && ln -sf /usr/local/bin/profile-perf.sh /usr/local/bin/profile-perf \
    && ln -sf /usr/local/bin/analyze-connections.sh /usr/local/bin/analyze-connections

# Thêm FlameGraph vào PATH để tiện thực thi lệnh ở mọi nơi
ENV PATH="/opt/FlameGraph:${PATH}"
ENV HOME=/tmp
