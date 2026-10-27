#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# Capsizer 2-Phase Capacity Sizing & Profiling Automation Script
# ==============================================================================
# Hỗ trợ 2 chế độ phát tải (Load Generator):
#   - local : Chạy bộ phát tải cmd/ramp trực tiếp trên máy này.
#   - remote: Tự động biên dịch và điều khiển máy phát tải từ xa qua SSH.
#
# Hỗ trợ 2 môi trường ứng dụng mục tiêu (Target):
#   - Local Host
#   - Kubernetes Pod (hỗ trợ 3 mode: debug, sidecar, cp)
# ==============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

# Tự động nạp file cấu hình môi trường nếu tồn tại
ENV_FILE="${PROJECT_ROOT}/.env.remote"
if [[ ! -f "$ENV_FILE" && -f "${PROJECT_ROOT}/.env" ]]; then
  ENV_FILE="${PROJECT_ROOT}/.env"
fi

if [[ -f "$ENV_FILE" ]]; then
  # shellcheck disable=SC1090
  source "$ENV_FILE"
fi

# ==============================================================================
# Giá trị mặc định
# ==============================================================================
URL="${TARGET_URL:-http://127.0.0.1:8000/chat}"
SCENARIO="${SCENARIO:-chat-sse}"
METHOD="${METHOD:-GET}"
BODY="${BODY:-}"
BODY_FILE="${BODY_FILE:-}"
STEPS="${STEPS:-2,4,8,16,32}"
STEP_SECONDS="${STEP_SECONDS:-30}"
BASELINE_WAIT="${BASELINE_WAIT:-60}"
COOLDOWN="${COOLDOWN:-15}"
SETTLE="${SETTLE:-60}"
TIMEOUT="${TIMEOUT:-120}"
MAX_INFLIGHT="${MAX_INFLIGHT:-2000}"
CACHE_BUST="${CACHE_BUST:-false}"
SKIP_VERIFY="${SKIP_VERIFY:-false}"
QUERIES_FILE="${QUERIES_FILE:-}"
USERS_FILE="${USERS_CSV:-${USERS_FILE:-}}"

PROFILE_TYPE="${PROFILE_TYPE:-pyspy}" # pyspy hoặc perf
POD_NAME="${K8S_POD:-}"
NAMESPACE="${K8S_NAMESPACE:-default}"
CONTAINER_NAME="${K8S_CONTAINER:-}"
K8S_MODE="${K8S_MODE:-debug}" # debug, sidecar, cp
SIDECAR_IMAGE="${SIDECAR_IMAGE:-docker.io/duyle95/profiling-sidecar:latest}"
SIDECAR_CONTAINER="${SIDECAR_CONTAINER:-sidecar}"
DEBUG_CONTAINER_NAME="capsizer-debugger"
PORT="${TARGET_PORT:-8000}"
TARGET_PID=""
OUT_DIR="${PROJECT_ROOT}/benchmark-results/$(date +%Y%m%d_%H%M%S)"

# Cấu hình máy phát tải (Load Generator)
LOADGEN_MODE="${LOADGEN_MODE:-local}" # 'local' hoặc 'remote'
LOADGEN_HOST="${REMOTE_HOST:-${LOADGEN_HOST:-}}"
LOADGEN_USER="${REMOTE_USER:-${LOADGEN_USER:-$(whoami)}}"
LOADGEN_PORT="${REMOTE_PORT:-${LOADGEN_PORT:-22}}"
LOADGEN_KEY="${REMOTE_KEY:-${LOADGEN_KEY:-}}"
LOADGEN_DIR="${REMOTE_DIR:-${LOADGEN_DIR:-~/capsizer-loadtest}}"

EXTRA_OPTS=()

# Parse cờ dòng lệnh (ghi đè cấu hình)
while [[ $# -gt 0 ]]; do
  case $1 in
    --url) URL="$2"; shift 2 ;;
    --scenario) SCENARIO="$2"; shift 2 ;;
    --method) METHOD="$2"; shift 2 ;;
    --body) BODY="$2"; shift 2 ;;
    --body-file) BODY_FILE="$2"; shift 2 ;;
    --steps) STEPS="$2"; shift 2 ;;
    --step-seconds) STEP_SECONDS="$2"; shift 2 ;;
    --baseline-wait) BASELINE_WAIT="$2"; shift 2 ;;
    --cooldown) COOLDOWN="$2"; shift 2 ;;
    --settle) SETTLE="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --max-inflight) MAX_INFLIGHT="$2"; shift 2 ;;
    --cache-bust) CACHE_BUST="true"; shift 1 ;;
    --skip-verify) SKIP_VERIFY="true"; shift 1 ;;
    --queries) QUERIES_FILE="$2"; shift 2 ;;
    --users) USERS_FILE="$2"; shift 2 ;;
    --opt) EXTRA_OPTS+=("-opt" "$2"); shift 2 ;;

    --profile-type) PROFILE_TYPE="$2"; shift 2 ;;
    --pod) POD_NAME="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --container) CONTAINER_NAME="$2"; shift 2 ;;
    --k8s-mode) K8S_MODE="$2"; shift 2 ;;
    --sidecar-image) SIDECAR_IMAGE="$2"; shift 2 ;;
    --sidecar-container) SIDECAR_CONTAINER="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --pid) TARGET_PID="$2"; shift 2 ;;
    --out-dir) OUT_DIR="$2"; shift 2 ;;

    --loadgen-mode) LOADGEN_MODE="$2"; shift 2 ;;
    --loadgen-host) LOADGEN_HOST="$2"; shift 2 ;;
    --loadgen-user) LOADGEN_USER="$2"; shift 2 ;;
    --loadgen-port) LOADGEN_PORT="$2"; shift 2 ;;
    --loadgen-key) LOADGEN_KEY="$2"; shift 2 ;;
    --loadgen-dir) LOADGEN_DIR="$2"; shift 2 ;;
    --env-file)
      ENV_FILE="$2"
      if [[ -f "$ENV_FILE" ]]; then
        # shellcheck disable=SC1090
        source "$ENV_FILE"
      fi
      shift 2 ;;
    -h|--help)
      echo "Sử dụng: $0 [OPTIONS]"
      echo ""
      echo "Tùy chọn tải & Kịch bản:"
      echo "  --url <url>                 Endpoint nhận request (default: $URL)"
      echo "  --scenario <name>           Kịch bản: chat-sse, sample-get, sample-post (default: $SCENARIO)"
      echo "  --method <method>           HTTP method cho REST (default: $METHOD)"
      echo "  --body <string>             Chuỗi body JSON/text"
      echo "  --body-file <file>          File body payload"
      echo "  --steps <csv>               Dải bậc RPS (default: $STEPS)"
      echo "  --step-seconds <sec>        Thời gian mỗi bậc phát tải (default: $STEP_SECONDS)"
      echo "  --baseline-wait <sec>       Thời gian chờ lấy mẫu CPU/RAM nền (default: $BASELINE_WAIT)"
      echo "  --cooldown <sec>            Nghỉ giữa hai bậc (default: $COOLDOWN)"
      echo "  --queries <file>            File câu hỏi cho chatbot"
      echo "  --users <file>              File users.csv"
      echo "  --cache-bust                Thêm tham số timestamp ngẫu nhiên chống cache"
      echo "  --skip-verify               Bỏ qua xác thực TLS/SSL"
      echo "  --profile-type <type>       Công cụ profiling: pyspy hoặc perf (default: $PROFILE_TYPE)"
      echo "  --out-dir <dir>             Thư mục lưu trữ artifact kết quả"
      echo ""
      echo "Tùy chọn Máy Phát Tải (Load Generator):"
      echo "  --loadgen-mode <mode>       'local' (chính máy này) hoặc 'remote' (qua SSH) (default: $LOADGEN_MODE)"
      echo "  --loadgen-host <host>       IP/Host máy phát tải từ xa (khi mode=remote)"
      echo "  --loadgen-user <user>       SSH User (default: $LOADGEN_USER)"
      echo "  --loadgen-port <port>       SSH Port (default: $LOADGEN_PORT)"
      echo "  --loadgen-key <file>        Đường dẫn SSH private key"
      echo "  --loadgen-dir <dir>         Thư mục làm việc trên máy bắn tải (default: $LOADGEN_DIR)"
      echo "  --env-file <file>           Đường dẫn file cấu hình .env / .env.remote"
      echo ""
      echo "Tùy chọn Kubernetes Mục Tiêu (Target):"
      echo "  --pod <pod_name>            Tên Pod mục tiêu trên K8s"
      echo "  --namespace <ns>            K8s Namespace (default: $NAMESPACE)"
      echo "  --container <name>          Tên container chứa app (để debug target)"
      echo "  --k8s-mode <mode>           'debug' (gắn Ephemeral container), 'sidecar', hoặc 'cp' (default: $K8S_MODE)"
      echo "  --sidecar-image <image>     Image profiling sidecar (default: $SIDECAR_IMAGE)"
      echo "  --sidecar-container <name>  Tên container sidecar có sẵn (default: $SIDECAR_CONTAINER)"
      echo "  --port <port>               Cổng TCP của ứng dụng (default: $PORT)"
      exit 0
      ;;
    *)
      echo "Tham số không hợp lệ: $1" >&2
      exit 1
      ;;
  esac
done

mkdir -p "$OUT_DIR"

RAMP_LOG="$OUT_DIR/phase1-ramp.jsonl"
PROBE_LOG="$OUT_DIR/phase1-probe.jsonl"
REPORT_LOG="$OUT_DIR/phase1-report.txt"

echo "================================================================================"
echo "    CAPSIZER 2-PHASE CAPACITY SIZING & PROFILING AUTOMATION"
echo "================================================================================"
echo "Mục tiêu test     : $URL (Scenario: $SCENARIO)"
echo "Dải bậc tải       : $STEPS (Mỗi bậc: ${STEP_SECONDS}s, Cooldown: ${COOLDOWN}s)"
echo "Thư mục output    : $OUT_DIR"

if [[ "$LOADGEN_MODE" == "remote" ]]; then
  echo "Máy phát tải      : REMOTE (Host: ${LOADGEN_USER}@${LOADGEN_HOST}:${LOADGEN_PORT})"
else
  echo "Máy phát tải      : LOCAL (Chính máy này)"
fi

if [[ -n "$POD_NAME" ]]; then
  echo "Môi trường Target : Kubernetes Pod ($POD_NAME, Namespace: $NAMESPACE)"
  echo "Chế độ K8s        : $K8S_MODE (Image: $SIDECAR_IMAGE)"
else
  echo "Môi trường Target : Local Host / External URL"
fi
echo "================================================================================"

# ==============================================================================
# HÀM ĐIỀU PHỐI MÁY PHÁT TẢI (LOCAL / REMOTE SSH)
# ==============================================================================

SSH_OPTS=(-p "$LOADGEN_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
if [[ -n "$LOADGEN_KEY" ]]; then
  SSH_OPTS+=(-i "$LOADGEN_KEY")
fi

run_ssh() {
  ssh "${SSH_OPTS[@]}" "${LOADGEN_USER}@${LOADGEN_HOST}" "$@"
}

run_scp_to() {
  local src="$1"
  local dst="$2"
  scp "${SSH_OPTS[@]}" "$src" "${LOADGEN_USER}@${LOADGEN_HOST}:$dst"
}

run_scp_from() {
  local src="$1"
  local dst="$2"
  scp "${SSH_OPTS[@]}" "$src" "${LOADGEN_USER}@${LOADGEN_HOST}:$dst"
}

setup_remote_loadgen() {
  if [[ -z "$LOADGEN_HOST" ]]; then
    echo "[ERROR] Chế độ phát tải remote yêu cầu --loadgen-host hoặc REMOTE_HOST trong .env.remote!" >&2
    exit 1
  fi

  echo "==> [Remote LoadGen] Kiểm tra kết nối SSH tới ${LOADGEN_USER}@${LOADGEN_HOST}..."
  if ! run_ssh "uname -srm" >/dev/null 2>&1; then
    echo "[ERROR] Không thể kết nối SSH tới ${LOADGEN_USER}@${LOADGEN_HOST}. Vui lòng kiểm tra lại SSH key và kết nối mạng." >&2
    exit 1
  fi
  local remote_os
  remote_os=$(run_ssh "uname -srm")
  echo "    ✓ Kết nối SSH thành công ($remote_os)."

  local bin_dir="${PROJECT_ROOT}/bin"
  mkdir -p "$bin_dir"
  local local_ramp_bin="${bin_dir}/capsizer-ramp-linux-amd64"

  echo "==> [Remote LoadGen] Biên dịch tĩnh cmd/ramp (Linux amd64)..."
  (
    cd "${PROJECT_ROOT}"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$local_ramp_bin" ./cmd/ramp
  )

  echo "==> [Remote LoadGen] Đồng bộ binary và cấu hình lên ${LOADGEN_DIR}..."
  run_ssh "mkdir -p '${LOADGEN_DIR}'"
  run_scp_to "$local_ramp_bin" "${LOADGEN_DIR}/capsizer-ramp"
  run_ssh "chmod +x '${LOADGEN_DIR}/capsizer-ramp'"

  if [[ -n "$QUERIES_FILE" && -f "$QUERIES_FILE" ]]; then
    run_scp_to "$QUERIES_FILE" "${LOADGEN_DIR}/queries.txt"
  fi
  if [[ -n "$USERS_FILE" && -f "$USERS_FILE" ]]; then
    run_scp_to "$USERS_FILE" "${LOADGEN_DIR}/users.csv"
  fi
  if [[ -n "$BODY_FILE" && -f "$BODY_FILE" ]]; then
    run_scp_to "$BODY_FILE" "${LOADGEN_DIR}/body_payload"
  fi
  echo "    ✓ Chuẩn bị máy phát tải hoàn tất."
}

# Hàm thực thi phát tải (trừu tượng hóa local và remote)
# Cú pháp: run_loadgen <steps> <step_seconds> <baseline_wait> <cooldown> <output_file> [is_background]
run_loadgen() {
  local l_steps="$1"
  local l_step_sec="$2"
  local l_base_wait="$3"
  local l_cooldown="$4"
  local l_out_file="$5"
  local l_bg="${6:-false}"

  if [[ "$LOADGEN_MODE" == "remote" ]]; then
    local r_cmd="${LOADGEN_DIR}/capsizer-ramp"
    r_cmd+=" -scenario '${SCENARIO}'"
    r_cmd+=" -url '${URL}'"
    r_cmd+=" -method '${METHOD}'"
    r_cmd+=" -steps '${l_steps}'"
    r_cmd+=" -step-seconds ${l_step_sec}"
    r_cmd+=" -baseline-wait ${l_base_wait}"
    r_cmd+=" -cooldown ${l_cooldown}"
    r_cmd+=" -settle ${SETTLE}"
    r_cmd+=" -timeout ${TIMEOUT}"
    r_cmd+=" -max-inflight ${MAX_INFLIGHT}"
    r_cmd+=" -out '${LOADGEN_DIR}/current-ramp.jsonl'"

    if [[ "$CACHE_BUST" == "true" ]]; then r_cmd+=" -cache-bust"; fi
    if [[ "$SKIP_VERIFY" == "true" ]]; then r_cmd+=" -skip-verify"; fi
    if [[ -n "$BODY" ]]; then r_cmd+=" -body '${BODY}'"; fi
    if [[ -n "$BODY_FILE" && -f "$BODY_FILE" ]]; then r_cmd+=" -body-file '${LOADGEN_DIR}/body_payload'"; fi
    if [[ -n "$QUERIES_FILE" && -f "$QUERIES_FILE" ]]; then r_cmd+=" -queries '${LOADGEN_DIR}/queries.txt'"; fi
    if [[ -n "$USERS_FILE" && -f "$USERS_FILE" ]]; then r_cmd+=" -users '${LOADGEN_DIR}/users.csv'"; fi
    for opt in "${EXTRA_OPTS[@]}"; do r_cmd+=" $opt"; done

    if [[ "$l_bg" == "true" ]]; then
      run_ssh "cd '${LOADGEN_DIR}' && ${r_cmd}" > "$OUT_DIR/remote-loadgen.log" 2>&1 &
      LOADGEN_BG_PID=$!
    else
      run_ssh "cd '${LOADGEN_DIR}' && ${r_cmd}"
      run_scp_from "${LOADGEN_DIR}/current-ramp.jsonl" "$l_out_file"
    fi
  else
    # Local mode
    local local_args=(
      -scenario "$SCENARIO"
      -url "$URL"
      -method "$METHOD"
      -steps "$l_steps"
      -step-seconds "$l_step_sec"
      -baseline-wait "$l_base_wait"
      -cooldown "$l_cooldown"
      -settle "$SETTLE"
      -timeout "$TIMEOUT"
      -max-inflight "$MAX_INFLIGHT"
      -out "$l_out_file"
    )
    if [[ "$CACHE_BUST" == "true" ]]; then local_args+=(-cache-bust); fi
    if [[ "$SKIP_VERIFY" == "true" ]]; then local_args+=(-skip-verify); fi
    if [[ -n "$BODY" ]]; then local_args+=(-body "$BODY"); fi
    if [[ -n "$BODY_FILE" && -f "$BODY_FILE" ]]; then local_args+=(-body-file "$BODY_FILE"); fi
    if [[ -n "$QUERIES_FILE" && -f "$QUERIES_FILE" ]]; then local_args+=(-queries "$QUERIES_FILE"); fi
    if [[ -n "$USERS_FILE" && -f "$USERS_FILE" ]]; then local_args+=(-users "$USERS_FILE"); fi
    if [[ ${#EXTRA_OPTS[@]} -gt 0 ]]; then local_args+=("${EXTRA_OPTS[@]}"); fi

    if [[ "$l_bg" == "true" ]]; then
      go run ./cmd/ramp "${local_args[@]}" > "$OUT_DIR/local-loadgen.log" 2>&1 &
      LOADGEN_BG_PID=$!
    else
      go run ./cmd/ramp "${local_args[@]}"
    fi
  fi
}

# ==============================================================================
# HÀM THỰC THI TRONG MÔI TRƯỜNG KUBERNETES TARGET
# ==============================================================================

setup_k8s_debug_container() {
  echo "==> Đang kiểm tra / gắn Ephemeral Debug Container ($DEBUG_CONTAINER_NAME) vào Pod $POD_NAME..."
  local TARGET_FLAG=""
  if [[ -n "$CONTAINER_NAME" ]]; then
    TARGET_FLAG="--target=$CONTAINER_NAME"
  fi

  local HAS_DEBUG
  HAS_DEBUG=$(kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath="{.spec.ephemeralContainers[*].name}" 2>/dev/null || true)
  if echo "$HAS_DEBUG" | grep -qw "$DEBUG_CONTAINER_NAME"; then
    echo "    ✓ Ephemeral container $DEBUG_CONTAINER_NAME đã sẵn sàng trong Pod."
  else
    echo "    Đang đính kèm container debug với quyền SYS_PTRACE..."
    kubectl debug "$POD_NAME" -n "$NAMESPACE" \
      --image="$SIDECAR_IMAGE" \
      --custom='{"spec":{"ephemeralContainers":[{"name":"'"$DEBUG_CONTAINER_NAME"'","image":"'"$SIDECAR_IMAGE"'","securityContext":{"capabilities":{"add":["SYS_PTRACE"]},"runAsUser":0},"stdin":true,"tty":true}]}}' \
      $TARGET_FLAG \
      --profile=sysadmin 2>/dev/null || \
    kubectl debug "$POD_NAME" -n "$NAMESPACE" \
      --image="$SIDECAR_IMAGE" \
      $TARGET_FLAG \
      --profile=sysadmin 2>/dev/null || \
    kubectl debug "$POD_NAME" -n "$NAMESPACE" \
      --image="$SIDECAR_IMAGE" \
      $TARGET_FLAG || true

    sleep 5
  fi
}

exec_in_target() {
  local CMD="$1"
  if [[ -z "$POD_NAME" ]]; then
    eval "$CMD"
  elif [[ "$K8S_MODE" == "debug" ]]; then
    kubectl exec -n "$NAMESPACE" "$POD_NAME" -c "$DEBUG_CONTAINER_NAME" -- sh -c "$CMD"
  elif [[ "$K8S_MODE" == "sidecar" ]]; then
    kubectl exec -n "$NAMESPACE" "$POD_NAME" -c "$SIDECAR_CONTAINER" -- sh -c "$CMD"
  elif [[ "$K8S_MODE" == "cp" ]]; then
    local C_FLAG=""
    if [[ -n "$CONTAINER_NAME" ]]; then
      C_FLAG="-c $CONTAINER_NAME"
    fi
    kubectl exec -n "$NAMESPACE" "$POD_NAME" $C_FLAG -- sh -c "$CMD"
  fi
}

copy_from_target() {
  local REMOTE_PATH="$1"
  local LOCAL_PATH="$2"
  if [[ -z "$POD_NAME" ]]; then
    cp "$REMOTE_PATH" "$LOCAL_PATH" 2>/dev/null || true
  elif [[ "$K8S_MODE" == "debug" ]]; then
    kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" "$LOCAL_PATH" -c "$DEBUG_CONTAINER_NAME" 2>/dev/null || true
  elif [[ "$K8S_MODE" == "sidecar" ]]; then
    kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" "$LOCAL_PATH" -c "$SIDECAR_CONTAINER" 2>/dev/null || true
  elif [[ "$K8S_MODE" == "cp" ]]; then
    local C_FLAG=""
    if [[ -n "$CONTAINER_NAME" ]]; then
      C_FLAG="-c $CONTAINER_NAME"
    fi
    kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" "$LOCAL_PATH" $C_FLAG 2>/dev/null || true
  fi
}

# ==============================================================================
# KHỞI CHẠY THIẾT LẬP MÁY BẮN TẢI
# ==============================================================================
if [[ "$LOADGEN_MODE" == "remote" ]]; then
  setup_remote_loadgen
fi

# ==============================================================================
# GIAI ĐOẠN 1: CAPACITY SIZING SẠCH (Ramp + Probe)
# ==============================================================================
echo ""
echo "▶ [PHA 1] KHỞI ĐỘNG ĐO CAPACITY SIZING SẠCH (KHÔNG BẬT PROFILER)..."

PROBE_PID=""
if [[ -n "$POD_NAME" ]]; then
  if [[ "$K8S_MODE" == "debug" ]]; then
    setup_k8s_debug_container
    echo "==> Đang khởi chạy probe từ Debug Container..."
    exec_in_target "nohup probe -port $PORT -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 &"
  elif [[ "$K8S_MODE" == "sidecar" ]]; then
    echo "==> Đang khởi chạy probe từ Sidecar Container ($SIDECAR_CONTAINER)..."
    exec_in_target "nohup probe -port $PORT -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 &"
  elif [[ "$K8S_MODE" == "cp" ]]; then
    echo "==> Đang biên dịch và copy static binary probe vào Pod..."
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o "$OUT_DIR/probe" ./cmd/probe
    local C_FLAG=""
    if [[ -n "$CONTAINER_NAME" ]]; then C_FLAG="-c $CONTAINER_NAME"; fi
    kubectl cp "$OUT_DIR/probe" "${NAMESPACE}/${POD_NAME}:/tmp/probe" $C_FLAG
    kubectl exec -n "$NAMESPACE" "$POD_NAME" $C_FLAG -- sh -c "nohup /tmp/probe -port $PORT -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 &"
  fi
else
  echo "==> Đang khởi chạy probe trên máy local..."
  go run ./cmd/probe -port "$PORT" -out "$PROBE_LOG" &
  PROBE_PID=$!
fi

sleep 2

# Phát tải Open-Loop Ramp Pha 1
echo "==> Bắt đầu phát tải Open-Loop (đếm lùi ${BASELINE_WAIT}s lấy mẫu nền)..."
run_loadgen "$STEPS" "$STEP_SECONDS" "$BASELINE_WAIT" "$COOLDOWN" "$RAMP_LOG" false

echo "==> Hoàn thành phát tải Pha 1!"

# Dừng probe và thu thập log
if [[ -n "$POD_NAME" ]]; then
  echo "==> Đang dừng probe và tải file log probe.jsonl về..."
  exec_in_target "pkill -f 'probe' || true"
  sleep 1
  copy_from_target "/tmp/probe.jsonl" "$PROBE_LOG"
else
  if [[ -n "$PROBE_PID" ]]; then
    kill -INT "$PROBE_PID" 2>/dev/null || true
    wait "$PROBE_PID" 2>/dev/null || true
  fi
fi

# Phân tích kết quả Pha 1 với cmd/report
echo ""
echo "==> Đang phân tích kết quả định chuẩn Pha 1..."
set +e
go run ./cmd/report -ramp "$RAMP_LOG" -probe "$PROBE_LOG" | tee "$REPORT_LOG"
set -e

# Tìm điểm gãy (Knee Point)
KNEE_RPS=$(grep -E "Điểm gãy quan sát được" "$REPORT_LOG" 2>/dev/null | grep -oE "[0-9]+(\.[0-9]+)? req/s" | head -n 1 | awk '{print $1}' || true)

if [[ -z "$KNEE_RPS" || "$KNEE_RPS" == "0" ]]; then
  echo ""
  echo "================================================================================"
  echo "✓ KẾT QUẢ PHA 1: HỆ THỐNG HOẠT ĐỘNG HOÀN TOÀN KHỎE MẠNH TRONG DẢI BẬC ĐÃ TEST!"
  echo "  Không phát hiện điểm gãy (Knee Point). Bạn có thể nâng dải --steps lên để"
  echo "  tìm ngưỡng giới hạn tối đa."
  echo "================================================================================"
  exit 0
fi

# ==============================================================================
# GIAI ĐOẠN 2: CHẨN ĐOÁN NGUYÊN NHÂN SÂU (FlameGraph Profiling)
# ==============================================================================
echo ""
echo "================================================================================"
echo "⚠️ PHÁT HIỆN ĐIỂM GÃY TẠI MỨC TẢI: ${KNEE_RPS} req/s"
echo "▶ [PHA 2] TỰ ĐỘNG CHUYỂN SANG CHẨN ĐOÁN NGUYÊN NHÂN SÂU (FLAMEGRAPH)..."
echo "================================================================================"
echo "Mức tải giữ ổn định : ${KNEE_RPS} req/s trong 45 giây"
echo "Công cụ profiling    : ${PROFILE_TYPE}"

SVG_OUT="$OUT_DIR/flamegraph_knee_${KNEE_RPS}rps.svg"
PHASE2_RAMP_LOG="$OUT_DIR/phase2-ramp.jsonl"

# Phát tải cố định tại Knee RPS ở background
LOADGEN_BG_PID=""
echo "==> Đang kích hoạt phát tải duy trì tại ${KNEE_RPS} req/s..."
run_loadgen "${KNEE_RPS}" 45 0 0 "$PHASE2_RAMP_LOG" true

sleep 5

# Kích hoạt Profiler trên target
if [[ -n "$POD_NAME" ]]; then
  echo "==> Đang kích hoạt profiling trong Kubernetes Pod..."
  if [[ "$PROFILE_TYPE" == "pyspy" ]]; then
    echo "    Chạy py-spy record (30s, rate 100Hz)..."
    exec_in_target "py-spy record -o /tmp/flamegraph.svg -d 30 --rate 100 --subprocesses || true"
    copy_from_target "/tmp/flamegraph.svg" "$SVG_OUT"
  else
    echo "    Chạy profile-perf (30s, rate 99Hz)..."
    exec_in_target "profile-perf 30 /tmp/flamegraph.svg || true"
    copy_from_target "/tmp/flamegraph.svg" "$SVG_OUT"
  fi
else
  echo "==> Đang kích hoạt profiling cục bộ..."
  if [[ "$PROFILE_TYPE" == "pyspy" ]] && command -v py-spy &>/dev/null; then
    TARGET_ARG=""
    if [[ -n "$TARGET_PID" ]]; then TARGET_ARG="-p $TARGET_PID"; else TARGET_ARG="--subprocesses"; fi
    py-spy record -o "$SVG_OUT" -d 30 --rate 100 $TARGET_ARG 2>/dev/null || true
  elif [[ "$PROFILE_TYPE" == "perf" ]] && command -v perf &>/dev/null; then
    perf record -F 99 -g -a -o "$OUT_DIR/perf.data" -- sleep 30 2>/dev/null || true
    if command -v flamegraph.pl &>/dev/null; then
      perf script -i "$OUT_DIR/perf.data" | stackcollapse-perf.pl | flamegraph.pl > "$SVG_OUT"
    fi
  else
    echo "    (Lưu ý: Không tìm thấy py-spy hoặc perf trên máy host. Bỏ qua xuất SVG cục bộ.)"
  fi
fi

# Chờ tiến trình phát tải Phase 2 kết thúc
if [[ -n "$LOADGEN_BG_PID" ]]; then
  wait "$LOADGEN_BG_PID" 2>/dev/null || true
fi

# Nếu là remote, tải file phase2 ramp về
if [[ "$LOADGEN_MODE" == "remote" ]]; then
  run_scp_from "${LOADGEN_DIR}/current-ramp.jsonl" "$PHASE2_RAMP_LOG" 2>/dev/null || true
fi

echo ""
echo "================================================================================"
echo "✓ HOÀN TẤT TOÀN DIỆN QUY TRÌNH 2 PHA ĐỊNH CHUẨN VÀ PROFILING!"
echo "================================================================================"
echo "Báo cáo phân tích Sizing : $REPORT_LOG"
if [[ -f "$SVG_OUT" ]]; then
  echo "Biểu đồ FlameGraph SVG   : $SVG_OUT"
  echo "▶ Hãy mở file SVG trên trình duyệt để soi chi tiết hàm/dòng code gây nghẽn tại bậc ${KNEE_RPS} req/s!"
fi
echo "================================================================================"
