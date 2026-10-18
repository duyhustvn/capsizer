# syntax=docker/dockerfile:1

# ==============================================================================
# STAGE 1: BASE IMAGE (Môi trường nền tảng chứa Go SDK + Core Ramps)
# ==============================================================================
# Dùng để:
# 1. Làm base image (`capsizer-ramp:base`) cho các Dockerfile kế thừa sau này
#    khi thêm kịch bản (scenario) mới.
# 2. Hoặc chạy container trực tiếp và mount mã nguồn scenario từ host mà KHÔNG cần
#    build lại image (Entrypoint sẽ tự động phát hiện và biên dịch lại ngay lập tức).
# ==============================================================================
FROM golang:alpine AS base

# Cài đặt các gói bổ trợ cần thiết: chứng chỉ TLS/SSL và múi giờ
RUN apk add --no-cache ca-certificates tzdata git

WORKDIR /app

# Copy module definition trước để tận dụng Docker layer cache
COPY ramps/golang/go.mod ./
RUN go mod download 2>/dev/null || true

# Copy toàn bộ mã nguồn của ramps/golang
COPY ramps/golang/ ./

# Copy các script bổ trợ
COPY docker/update_loader.sh /usr/local/bin/update_loader
RUN chmod +x /usr/local/bin/update_loader

COPY docker/build_ramp.sh /usr/local/bin/build_ramp
RUN chmod +x /usr/local/bin/build_ramp

COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Biên dịch sẵn binary ban đầu với các kịch bản mặc định và lưu mã băm sha256
RUN build_ramp /app

# Thư mục làm việc chứa dữ liệu input (queries, users.csv, .env) và output (*.jsonl)
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["--help"]

# ==============================================================================
# STAGE 2: RUNNER (Image siêu nhẹ dùng cho Production / CI/CD)
# ==============================================================================
# Chỉ chứa binary tĩnh đã biên dịch (~15MB), không chứa Golang SDK.
# Dùng khi chỉ chạy các kịch bản có sẵn (chat-sse, rest) mà không cần thêm code mới.
# ==============================================================================
FROM alpine:3.20 AS runner

RUN apk add --no-cache ca-certificates tzdata

COPY --from=base /usr/local/bin/capsizer-ramp /usr/local/bin/capsizer-ramp

WORKDIR /data

ENTRYPOINT ["/usr/local/bin/capsizer-ramp"]
CMD ["--help"]
