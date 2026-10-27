# syntax=docker/dockerfile:1

# ==============================================================================
# STAGE 1: BASE IMAGE (Môi trường nền tảng chứa Go SDK + Capsizer Source)
# ==============================================================================
# Dùng để:
# 1. Làm base image (`capsizer-ramp:base`) khi phát triển / mount scenarios từ host.
# 2. Chạy báo cáo (report) trực tiếp mà không cần cài Go trên máy host.
# ==============================================================================
FROM golang:alpine AS base

RUN apk add --no-cache ca-certificates tzdata git

WORKDIR /app

# Copy module definition
COPY go.mod ./
RUN go mod download 2>/dev/null || true

# Copy mã nguồn core
COPY pkg/ ./pkg/
COPY cmd/ ./cmd/

# Copy các script bổ trợ
COPY docker/update_loader.sh /usr/local/bin/update_loader
RUN chmod +x /usr/local/bin/update_loader

COPY docker/build_ramp.sh /usr/local/bin/build_ramp
RUN chmod +x /usr/local/bin/build_ramp

COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Biên dịch sẵn các binary
RUN build_ramp /app
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /usr/local/bin/capsizer-probe ./cmd/probe
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /usr/local/bin/capsizer-report ./cmd/report

# Thư mục làm việc chứa dữ liệu input (queries, users.csv, .env) và output (*.jsonl)
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["--help"]

# ==============================================================================
# STAGE 2: RUNNER (Image siêu nhẹ dùng cho Production / CI/CD)
# ==============================================================================
# Chỉ chứa các binary tĩnh đã biên dịch (~20MB), không chứa Golang SDK.
# ==============================================================================
FROM alpine:3.20 AS runner

RUN apk add --no-cache ca-certificates tzdata

COPY --from=base /usr/local/bin/capsizer-ramp /usr/local/bin/capsizer-ramp
COPY --from=base /usr/local/bin/capsizer-probe /usr/local/bin/capsizer-probe
COPY --from=base /usr/local/bin/capsizer-report /usr/local/bin/capsizer-report

WORKDIR /data

ENTRYPOINT ["/usr/local/bin/capsizer-ramp"]
CMD ["--help"]
