#!/usr/bin/env bash
# ==============================================================================
# Script điều phối máy phát tải từ xa qua SSH cho Capsizer
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

# Nạp file cấu hình môi trường nếu có
ENV_FILE="${PROJECT_ROOT}/.env.remote"
if [[ -f "$ENV_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$ENV_FILE"
fi

# Giá trị mặc định
REMOTE_HOST="${REMOTE_HOST:-}"
REMOTE_USER="${REMOTE_USER:-$(whoami)}"
REMOTE_PORT="${REMOTE_PORT:-22}"
REMOTE_KEY="${REMOTE_KEY:-}"
REMOTE_DIR="${REMOTE_DIR:-~/capsizer-loadtest}"

TARGET_URL="${TARGET_URL:-http://127.0.0.1:8000/chat}"
SCENARIO="${SCENARIO:-chat-sse}"
STEPS="${STEPS:-1,2,4,8,16}"
STEP_SECONDS="${STEP_SECONDS:-60}"
COOLDOWN="${COOLDOWN:-20}"
SETTLE="${SETTLE:-60}"
TIMEOUT="${TIMEOUT:-120}"
MAX_INFLIGHT="${MAX_INFLIGHT:-2000}"
BASELINE_WAIT="${BASELINE_WAIT:-90}"
CACHE_BUST="${CACHE_BUST:-false}"
STOP_ON_KNEE="${STOP_ON_KNEE:-false}"
QUERIES_FILE="${QUERIES_FILE:-}"
USERS_CSV="${USERS_CSV:-}"
JWT_SECRET="${JWT_SECRET:-}"
JWT_TOKEN="${JWT_TOKEN:-}"

OUTPUT_DIR="${OUTPUT_DIR:-${PROJECT_ROOT}/results/run_$(date +%Y%m%d_%H%M%S)}"

# Hàm SSH/SCP tiện ích
SSH_OPTS=(-p "$REMOTE_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
if [[ -n "$REMOTE_KEY" ]]; then
    SSH_OPTS+=(-i "$REMOTE_KEY")
fi

run_ssh() {
    ssh "${SSH_OPTS[@]}" "${REMOTE_USER}@${REMOTE_HOST}" "$@"
}

run_scp_to() {
    local src="$1"
    local dst="$2"
    scp "${SSH_OPTS[@]}" "$src" "${REMOTE_USER}@${REMOTE_HOST}:$dst"
}

run_scp_from() {
    local src="$1"
    local dst="$2"
    scp "${SSH_OPTS[@]}" "${REMOTE_USER}@${REMOTE_HOST}:$src" "$dst"
}

check_ssh() {
    if [[ -z "$REMOTE_HOST" ]]; then
        echo "[ERROR] REMOTE_HOST chưa được cấu hình. Vui lòng khai báo trong .env.remote hoặc biến môi trường." >&2
        return 1
    fi
    echo "[INFO] Kiểm tra kết nối SSH tới ${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_PORT}..."
    if run_ssh "uname -srm" >/dev/null 2>&1; then
        local remote_info
        remote_info=$(run_ssh "uname -srm")
        echo "[SUCCESS] Kết nối SSH thành công! Remote OS: ${remote_info}"
        return 0
    else
        echo "[ERROR] Không thể kết nối SSH tới ${REMOTE_USER}@${REMOTE_HOST}." >&2
        echo "Vui lòng kiểm tra lại địa chỉ IP, SSH key (ssh-copy-id) và tường lửa." >&2
        return 1
    fi
}

build_and_deploy_binary() {
    echo "[INFO] Chuẩn bị bộ phát tải tĩnh cho máy từ xa..."
    local bin_dir="${PROJECT_ROOT}/bin"
    mkdir -p "$bin_dir"
    local local_bin="${bin_dir}/capsizer-ramp-linux-amd64"

    echo "[INFO] Biên dịch tĩnh ramps/golang (Linux amd64)..."
    (
        cd "${PROJECT_ROOT}/ramps/golang"
        CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$local_bin" .
    )

    echo "[INFO] Khởi tạo thư mục ${REMOTE_DIR} trên máy phát tải..."
    run_ssh "mkdir -p '${REMOTE_DIR}'"

    echo "[INFO] Đẩy binary lên máy phát tải..."
    run_scp_to "$local_bin" "${REMOTE_DIR}/capsizer-ramp"
    run_ssh "chmod +x '${REMOTE_DIR}/capsizer-ramp'"

    # Đẩy file queries nếu có
    if [[ -n "$QUERIES_FILE" && -f "$QUERIES_FILE" ]]; then
        echo "[INFO] Đẩy file câu hỏi: $QUERIES_FILE"
        run_scp_to "$QUERIES_FILE" "${REMOTE_DIR}/queries.txt"
    fi

    # Đẩy file users.csv nếu có
    if [[ -n "$USERS_CSV" && -f "$USERS_CSV" ]]; then
        echo "[INFO] Đẩy file user pool: $USERS_CSV"
        run_scp_to "$USERS_CSV" "${REMOTE_DIR}/users.csv"
    fi
}

run_loadtest() {
    check_ssh
    build_and_deploy_binary

    mkdir -p "$OUTPUT_DIR"
    echo "[INFO] Thư mục lưu kết quả cục bộ: $OUTPUT_DIR"

    # Dựng tham số chạy
    local remote_cmd="${REMOTE_DIR}/capsizer-ramp"
    remote_cmd+=" -scenario '${SCENARIO}'"
    remote_cmd+=" -url '${TARGET_URL}'"
    remote_cmd+=" -steps '${STEPS}'"
    remote_cmd+=" -step-seconds ${STEP_SECONDS}"
    remote_cmd+=" -cooldown ${COOLDOWN}"
    remote_cmd+=" -settle ${SETTLE}"
    remote_cmd+=" -timeout ${TIMEOUT}"
    remote_cmd+=" -max-inflight ${MAX_INFLIGHT}"
    remote_cmd+=" -baseline-wait ${BASELINE_WAIT}"
    remote_cmd+=" -out '${REMOTE_DIR}/loadtest-ramp.jsonl'"

    if [[ "$CACHE_BUST" == "true" ]]; then
        remote_cmd+=" -cache-bust"
    fi
    if [[ "$STOP_ON_KNEE" == "true" ]]; then
        remote_cmd+=" -stop-on-knee"
    fi
    if [[ -n "$QUERIES_FILE" && -f "$QUERIES_FILE" ]]; then
        remote_cmd+=" -queries '${REMOTE_DIR}/queries.txt'"
    fi
    if [[ -n "$USERS_CSV" && -f "$USERS_CSV" ]]; then
        remote_cmd+=" -users-csv '${REMOTE_DIR}/users.csv'"
    fi
    if [[ -n "$JWT_SECRET" ]]; then
        remote_cmd+=" -jwt-secret '${JWT_SECRET}'"
    fi
    if [[ -n "$JWT_TOKEN" ]]; then
        remote_cmd+=" -jwt '${JWT_TOKEN}'"
    fi

    echo "================================================================================"
    echo "[START] BẮT ĐẦU PHÁT TẢI TỪ MÁY TỪ XA: ${REMOTE_HOST}"
    echo "Endpoint: $TARGET_URL | Kịch bản: $SCENARIO | Bậc: $STEPS"
    echo "================================================================================"

    set +e
    run_ssh "cd '${REMOTE_DIR}' && ${remote_cmd}"
    local exit_code=$?
    set -e

    if [[ $exit_code -ne 0 ]]; then
        echo "[WARNING] Quá trình phát tải kết thúc với mã thoát: $exit_code"
    else
        echo "[SUCCESS] Phát tải hoàn tất thành công!"
    fi

    echo "[INFO] Tải file loadtest-ramp.jsonl về máy cục bộ..."
    run_scp_from "${REMOTE_DIR}/loadtest-ramp.jsonl" "${OUTPUT_DIR}/loadtest-ramp.jsonl"

    echo "[INFO] Kết quả loadtest đã lưu tại: ${OUTPUT_DIR}/loadtest-ramp.jsonl"
    echo "${OUTPUT_DIR}" > "${PROJECT_ROOT}/.last_run_dir"
}

# Điều hướng cờ dòng lệnh
ACTION="${1:---run}"
case "$ACTION" in
    --check|check)
        check_ssh
        ;;
    --build|build)
        build_and_deploy_binary
        ;;
    --run|run)
        run_loadtest
        ;;
    *)
        echo "Cú pháp: $0 [--check | --build | --run]"
        exit 1
        ;;
esac
