#!/usr/bin/env bash
set -e

# ==============================================================================
# Capsizer 2-Phase Capacity Sizing & Profiling Automation Script
# ==============================================================================
# Hỗ trợ 2 môi trường: Local Host và Kubernetes Pod
# Trên Kubernetes hỗ trợ 3 chế độ:
#   1. debug (Mặc định & Khuyên dùng): Dùng Ephemeral Debug Container gắn
#      profiling-sidecar (đã có sẵn probe, py-spy, perf, FlameGraph) vào Pod.
#   2. sidecar: Dùng container sidecar đã triển khai sẵn trong Pod spec.
#   3. cp: Chỉ copy binary tĩnh probe vào pod (chỉ dùng cho Pha 1 đo sizing).
# ==============================================================================

# Giá trị mặc định
URL="http://127.0.0.1:8000/chat"
SCENARIO="chat-sse"
STEPS="2,4,8,16,32"
STEP_SECONDS=30
BASELINE_WAIT=60
COOLDOWN=15
PROFILE_TYPE="pyspy" # pyspy hoặc perf
POD_NAME=""
NAMESPACE="default"
CONTAINER_NAME=""
K8S_MODE="debug" # debug, sidecar, cp
SIDECAR_IMAGE="docker.io/duyle95/profiling-sidecar:latest"
SIDECAR_CONTAINER="sidecar"
DEBUG_CONTAINER_NAME="capsizer-debugger"
PORT=8000
TARGET_PID=""
OUT_DIR="./benchmark-results/$(date +%Y%m%d_%H%M%S)"

# Parse cờ dòng lệnh
while [[ $# -gt 0 ]]; do
  case $1 in
    --url) URL="$2"; shift 2 ;;
    --scenario) SCENARIO="$2"; shift 2 ;;
    --steps) STEPS="$2"; shift 2 ;;
    --step-seconds) STEP_SECONDS="$2"; shift 2 ;;
    --baseline-wait) BASELINE_WAIT="$2"; shift 2 ;;
    --cooldown) COOLDOWN="$2"; shift 2 ;;
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
    -h|--help)
      echo "Sử dụng: $0 [OPTIONS]"
      echo ""
      echo "Tùy chọn chung:"
      echo "  --url <url>                 Endpoint nhận request (default: $URL)"
      echo "  --scenario <name>           Kịch bản kiểm thử: chat-sse, sample-get, sample-post (default: $SCENARIO)"
      echo "  --steps <csv>               Dải bậc RPS (default: $STEPS)"
      echo "  --step-seconds <sec>        Thời gian mỗi bậc phát tải (default: $STEP_SECONDS)"
      echo "  --baseline-wait <sec>       Thời gian chờ lấy mẫu CPU/RAM nền (default: $BASELINE_WAIT)"
      echo "  --cooldown <sec>            Nghỉ giữa hai bậc (default: $COOLDOWN)"
      echo "  --profile-type <type>       Công cụ profiling: pyspy hoặc perf (default: $PROFILE_TYPE)"
      echo "  --out-dir <dir>             Thư mục lưu trữ artifact kết quả"
      echo ""
      echo "Tùy chọn Kubernetes:"
      echo "  --pod <pod_name>            Tên Pod mục tiêu trên K8s"
      echo "  --namespace <ns>            K8s Namespace (default: $NAMESPACE)"
      echo "  --container <name>          Tên container chứa app (để debug target)"
      echo "  --k8s-mode <mode>           Chế độ: 'debug' (gắn Ephemeral container), 'sidecar', hoặc 'cp' (default: $K8S_MODE)"
      echo "  --sidecar-image <image>     Image profiling sidecar (default: $SIDECAR_IMAGE)"
      echo "  --sidecar-container <name>  Tên container sidecar có sẵn nếu dùng mode 'sidecar' (default: $SIDECAR_CONTAINER)"
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
if [ -n "$POD_NAME" ]; then
  echo "Môi trường        : Kubernetes Pod ($POD_NAME, Namespace: $NAMESPACE)"
  echo "Chế độ K8s        : $K8S_MODE (Image: $SIDECAR_IMAGE)"
else
  echo "Môi trường        : Local Host"
fi
echo "================================================================================"

# ==============================================================================
# HÀM THỰC THI TRONG MÔI TRƯỜNG KUBERNETES
# ==============================================================================

setup_k8s_debug_container() {
  echo "==> Đang kiểm tra / gắn Ephemeral Debug Container ($DEBUG_CONTAINER_NAME) vào Pod $POD_NAME..."
  TARGET_FLAG=""
  if [ -n "$CONTAINER_NAME" ]; then
    TARGET_FLAG="--target=$CONTAINER_NAME"
  fi

  # Kiểm tra xem debug container đã tồn tại chưa
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

    # Đợi 5s cho debug container chuyển sang Running
    sleep 5
  fi
}

exec_in_target() {
  local CMD="$1"
  if [ -z "$POD_NAME" ]; then
    eval "$CMD"
  elif [ "$K8S_MODE" = "debug" ]; then
    kubectl exec -n "$NAMESPACE" "$POD_NAME" -c "$DEBUG_CONTAINER_NAME" -- sh -c "$CMD"
  elif [ "$K8S_MODE" = "sidecar" ]; then
    kubectl exec -n "$NAMESPACE" "$POD_NAME" -c "$SIDECAR_CONTAINER" -- sh -c "$CMD"
  elif [ "$K8S_MODE" = "cp" ]; then
    local C_FLAG=""
    if [ -n "$CONTAINER_NAME" ]; then
      C_FLAG="-c $CONTAINER_NAME"
    fi
    kubectl exec -n "$NAMESPACE" "$POD_NAME" $C_FLAG -- sh -c "$CMD"
  fi
}

copy_from_target() {
  local REMOTE_PATH="$1"
  local LOCAL_PATH="$2"
  if [ -z "$POD_NAME" ]; then
    cp "$REMOTE_PATH" "$LOCAL_PATH" 2>/dev/null || true
  elif [ "$K8S_MODE" = "debug" ]; then
    kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" "$LOCAL_PATH" -c "$DEBUG_CONTAINER_NAME" 2>/dev/null || true
  elif [ "$K8S_MODE" = "sidecar" ]; then
    kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" "$LOCAL_PATH" -c "$SIDECAR_CONTAINER" 2>/dev/null || true
  elif [ "$K8S_MODE" = "cp" ]; then
    local C_FLAG=""
    if [ -n "$CONTAINER_NAME" ]; then
      C_FLAG="-c $CONTAINER_NAME"
    fi
    kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" "$LOCAL_PATH" $C_FLAG 2>/dev/null || true
  fi
}

# ==============================================================================
# GIAI ĐOẠN 1: CAPACITY SIZING SẠCH (Ramp + Probe)
# ==============================================================================
echo ""
echo "▶ [PHA 1] KHỞI ĐỘNG ĐO CAPACITY SIZING SẠCH (KHÔNG BẬT PROFILER)..."

PROBE_PID=""
if [ -n "$POD_NAME" ]; then
  if [ "$K8S_MODE" = "debug" ]; then
    setup_k8s_debug_container
    echo "==> Đang khởi chạy probe từ Debug Container..."
    exec_in_target "nohup probe -port $PORT -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 &"
  elif [ "$K8S_MODE" = "sidecar" ]; then
    echo "==> Đang khởi chạy probe từ Sidecar Container ($SIDECAR_CONTAINER)..."
    exec_in_target "nohup probe -port $PORT -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 &"
  elif [ "$K8S_MODE" = "cp" ]; then
    echo "==> Đang biên dịch và copy static binary probe vào Pod..."
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o "$OUT_DIR/probe" ./cmd/probe
    local C_FLAG=""
    if [ -n "$CONTAINER_NAME" ]; then C_FLAG="-c $CONTAINER_NAME"; fi
    kubectl cp "$OUT_DIR/probe" "${NAMESPACE}/${POD_NAME}:/tmp/probe" $C_FLAG
    kubectl exec -n "$NAMESPACE" "$POD_NAME" $C_FLAG -- sh -c "nohup /tmp/probe -port $PORT -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 &"
  fi
else
  echo "==> Đang khởi chạy probe trên máy local..."
  go run ./cmd/probe -port "$PORT" -out "$PROBE_LOG" &
  PROBE_PID=$!
fi

sleep 2

# Phát tải Open-Loop Ramp
echo "==> Bắt đầu phát tải Open-Loop (đếm lùi ${BASELINE_WAIT}s lấy mẫu nền)..."
go run ./cmd/ramp \
  -scenario "$SCENARIO" \
  -url "$URL" \
  -steps "$STEPS" \
  -step-seconds "$STEP_SECONDS" \
  -baseline-wait "$BASELINE_WAIT" \
  -cooldown "$COOLDOWN" \
  -out "$RAMP_LOG"

echo "==> Hoàn thành phát tải Pha 1!"

# Dừng probe và lấy log
if [ -n "$POD_NAME" ]; then
  echo "==> Đang dừng probe và tải file log probe.jsonl về..."
  exec_in_target "pkill -f 'probe' || true"
  sleep 1
  copy_from_target "/tmp/probe.jsonl" "$PROBE_LOG"
else
  if [ -n "$PROBE_PID" ]; then
    kill -INT "$PROBE_PID" 2>/dev/null || true
    wait "$PROBE_PID" 2>/dev/null || true
  fi
fi

# Phân tích Pha 1
echo ""
echo "==> Đang phân tích kết quả định chuẩn Pha 1..."
go run ./cmd/report -ramp "$RAMP_LOG" -probe "$PROBE_LOG" | tee "$REPORT_LOG"

# Tìm Knee Point
KNEE_RPS=$(grep -E "Điểm gãy quan sát được" "$REPORT_LOG" | grep -oE "[0-9]+(\.[0-9]+)? req/s" | head -n 1 | awk '{print $1}')

if [ -z "$KNEE_RPS" ] || [ "$KNEE_RPS" = "0" ]; then
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

# Phát tải cố định tại Knee RPS ở background
PHASE2_RAMP_LOG="$OUT_DIR/phase2-ramp.jsonl"
go run ./cmd/ramp \
  -scenario "$SCENARIO" \
  -url "$URL" \
  -steps "${KNEE_RPS}" \
  -step-seconds 45 \
  -baseline-wait 0 \
  -cooldown 0 \
  -out "$PHASE2_RAMP_LOG" &
PHASE2_RAMP_PID=$!

sleep 5

# Kích hoạt Profiler
if [ -n "$POD_NAME" ]; then
  echo "==> Đang kích hoạt profiling trong Kubernetes Pod..."
  if [ "$PROFILE_TYPE" = "pyspy" ]; then
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
  if [ "$PROFILE_TYPE" = "pyspy" ] && command -v py-spy &>/dev/null; then
    TARGET_ARG=""
    if [ -n "$TARGET_PID" ]; then TARGET_ARG="-p $TARGET_PID"; else TARGET_ARG="--subprocesses"; fi
    py-spy record -o "$SVG_OUT" -d 30 --rate 100 $TARGET_ARG 2>/dev/null || true
  elif [ "$PROFILE_TYPE" = "perf" ] && command -v perf &>/dev/null; then
    perf record -F 99 -g -a -o "$OUT_DIR/perf.data" -- sleep 30 2>/dev/null || true
    if command -v flamegraph.pl &>/dev/null; then
      perf script -i "$OUT_DIR/perf.data" | stackcollapse-perf.pl | flamegraph.pl > "$SVG_OUT"
    fi
  else
    echo "    (Lưu ý: Không tìm thấy py-spy hoặc perf trên máy host. Bỏ qua xuất SVG cục bộ.)"
  fi
fi

# Chờ tải Phase 2 kết thúc
wait "$PHASE2_RAMP_PID" 2>/dev/null || true

echo ""
echo "================================================================================"
echo "✓ HOÀN TẤT TOÀN DIỆN QUY TRÌNH 2 PHA ĐỊNH CHUẨN VÀ PROFILING!"
echo "================================================================================"
echo "Báo cáo phân tích Sizing : $REPORT_LOG"
if [ -f "$SVG_OUT" ]; then
  echo "Biểu đồ FlameGraph SVG   : $SVG_OUT"
  echo "▶ Hãy mở file SVG trên trình duyệt để soi chi tiết hàm/dòng code gây nghẽn tại bậc ${KNEE_RPS} req/s!"
fi
echo "================================================================================"
