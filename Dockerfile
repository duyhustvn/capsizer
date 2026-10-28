# syntax=docker/dockerfile:1

# ==============================================================================
# STAGE 1: BUILDER
# ==============================================================================
FROM golang:alpine AS builder

RUN apk add --no-cache ca-certificates tzdata git

WORKDIR /app

# Copy module definition
COPY go.mod ./
RUN go mod download 2>/dev/null || true

# Copy mã nguồn core
COPY pkg/ ./pkg/
COPY cmd/ ./cmd/

# Biên dịch tĩnh toàn bộ 3 binary
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /usr/local/bin/capsizer-ramp ./cmd/ramp
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /usr/local/bin/capsizer-probe ./cmd/probe
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /usr/local/bin/capsizer-report ./cmd/report

# ==============================================================================
# STAGE 2: RUNNER (Image siêu nhẹ ~15MB dùng cho Production / CI/CD)
# ==============================================================================
FROM alpine:3.20 AS runner

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /usr/local/bin/capsizer-ramp /usr/local/bin/capsizer-ramp
COPY --from=builder /usr/local/bin/capsizer-probe /usr/local/bin/capsizer-probe
COPY --from=builder /usr/local/bin/capsizer-report /usr/local/bin/capsizer-report

# Alias tiện ích
RUN ln -s /usr/local/bin/capsizer-ramp /usr/local/bin/capsizer && \
    ln -s /usr/local/bin/capsizer-probe /usr/local/bin/probe && \
    ln -s /usr/local/bin/capsizer-report /usr/local/bin/report

WORKDIR /data

ENTRYPOINT ["/usr/local/bin/capsizer-ramp"]
CMD ["--help"]
