#!/usr/bin/env bash
# ==============================================================================
# Script điều phối daemon probe.go trên Pod / Container / Máy mục tiêu
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

ENV_FILE="${PROJECT_ROOT}/.env.remote"
if [[ -f "$ENV_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$ENV_FILE"
fi

TARGET_TYPE="${TARGET_TYPE:-k8s}"
K8S_NAMESPACE="${K8S_NAMESPACE:-default}"
K8S_POD="${K8S_POD:-}"
K8S_SELECTOR="${K8S_SELECTOR:-}"
TARGET_PORT="${TARGET_PORT:-8000}"

resolve_pod() {
    if [[ -z "$K8S_POD" ]]; then
        if [[ -n "$K8S_SELECTOR" ]]; then
            echo "[INFO] Đang tìm pod theo selector: -l $K8S_SELECTOR trong namespace $K8S_NAMESPACE..."
            K8S_POD=$(kubectl get pods -n "$K8S_NAMESPACE" -l "$K8S_SELECTOR" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        fi
    fi

    if [[ -z "$K8S_POD" ]]; then
        echo "[ERROR] K8S_POD chưa được cấu hình. Vui lòng khai báo K8S_POD hoặc K8S_SELECTOR trong .env.remote." >&2
        return 1
    fi
    echo "[INFO] Pod mục tiêu: ${K8S_NAMESPACE}/${K8S_POD}"
}

build_probe() {
    local bin_dir="${PROJECT_ROOT}/bin"
    mkdir -p "$bin_dir"
    local local_probe="${bin_dir}/probe-linux-amd64"
    if [[ ! -f "$local_probe" ]]; then
        echo "[INFO] Biên dịch tĩnh probe.go cho Linux amd64..."
        (
            cd "${PROJECT_ROOT}"
            CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$local_probe" probe.go
        )
    fi
}

start_probe() {
    build_probe
    local local_probe="${PROJECT_ROOT}/bin/probe-linux-amd64"

    if [[ "$TARGET_TYPE" == "k8s" ]]; then
        resolve_pod
        echo "[INFO] Đẩy probe vào container mục tiêu: ${K8S_NAMESPACE}/${K8S_POD}:/tmp/probe"
        kubectl cp "$local_probe" "${K8S_NAMESPACE}/${K8S_POD}:/tmp/probe"
        kubectl exec -n "$K8S_NAMESPACE" "$K8S_POD" -- chmod +x /tmp/probe

        echo "[INFO] Khởi động daemon probe ngầm trên port ${TARGET_PORT}..."
        kubectl exec -n "$K8S_NAMESPACE" "$K8S_POD" -- /bin/sh -c "nohup /tmp/probe --port ${TARGET_PORT} --out /tmp/probe.jsonl > /tmp/probe.log 2>&1 & echo \$! > /tmp/probe.pid"
        echo "[SUCCESS] Probe đã khởi động thành công trên pod (PID lưu tại /tmp/probe.pid)!"
    else
        echo "[INFO] Khởi động probe trên host nội bộ hoặc qua SSH..."
        nohup "$local_probe" --port "$TARGET_PORT" --out /tmp/probe.jsonl > /tmp/probe.log 2>&1 &
        echo $! > /tmp/probe.pid
        echo "[SUCCESS] Probe đã chạy cục bộ (PID: $(cat /tmp/probe.pid))"
    fi
}

stop_and_fetch_probe() {
    local out_dir="${1:-}"
    if [[ -z "$out_dir" ]]; then
        if [[ -f "${PROJECT_ROOT}/.last_run_dir" ]]; then
            out_dir=$(cat "${PROJECT_ROOT}/.last_run_dir")
        else
            out_dir="${PROJECT_ROOT}/results/latest"
        fi
    fi
    mkdir -p "$out_dir"

    if [[ "$TARGET_TYPE" == "k8s" ]]; then
        resolve_pod
        echo "[INFO] Dừng daemon probe trên ${K8S_NAMESPACE}/${K8S_POD}..."
        kubectl exec -n "$K8S_NAMESPACE" "$K8S_POD" -- /bin/sh -c "kill \$(cat /tmp/probe.pid 2>/dev/null) 2>/dev/null || pkill probe || true"

        echo "[INFO] Tải probe.jsonl từ pod về: ${out_dir}/probe.jsonl..."
        kubectl cp "${K8S_NAMESPACE}/${K8S_POD}:/tmp/probe.jsonl" "${out_dir}/probe.jsonl"
        echo "[SUCCESS] Đã lưu file probe: ${out_dir}/probe.jsonl"
    else
        echo "[INFO] Dừng probe..."
        kill "$(cat /tmp/probe.pid 2>/dev/null)" 2>/dev/null || pkill probe || true
        cp /tmp/probe.jsonl "${out_dir}/probe.jsonl"
        echo "[SUCCESS] Đã lưu file probe: ${out_dir}/probe.jsonl"
    fi
}

ACTION="${1:-start}"
case "$ACTION" in
    start)
        start_probe
        ;;
    stop)
        stop_and_fetch_probe "${2:-}"
        ;;
    *)
        echo "Cú pháp: $0 [start | stop [OUTPUT_DIR]]"
        exit 1
        ;;
esac
