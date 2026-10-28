#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# Capsizer 2-Phase Capacity Sizing & Profiling Automation Script
# ==============================================================================
# Kiến trúc 3 Máy (3-Node Topology):
#   1. Máy chạy Skill (Orchestrator Host - nơi đang chạy script này)
#   2. Máy phát tải (Load Generator Host - phát tải Open-Loop qua SSH)
#   3. Server Cụm K8s (K8s Master / Bastion Host - SSH passwordless + sudo root)
# ==============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

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

# ------------------------------------------------------------------------------
# Machine 2: Máy phát tải (Load Generator Host qua SSH)
# ------------------------------------------------------------------------------
LOADGEN_MODE="${LOADGEN_MODE:-local}" # 'local' hoặc 'remote'
LOADGEN_HOST="${REMOTE_HOST:-${LOADGEN_HOST:-}}"
LOADGEN_USER="${REMOTE_USER:-${LOADGEN_USER:-$(whoami)}}"
LOADGEN_PORT="${REMOTE_PORT:-${LOADGEN_PORT:-22}}"
LOADGEN_KEY="${REMOTE_KEY:-${LOADGEN_KEY:-}}"
LOADGEN_DIR="${REMOTE_DIR:-${LOADGEN_DIR:-~/capsizer-loadtest}}"

# ------------------------------------------------------------------------------
# Machine 3: Server cụm Kubernetes (K8s Master/Bastion qua SSH hoặc local)
# ------------------------------------------------------------------------------
K8S_SSH_HOST="${K8S_SSH_HOST:-}"      # IP hoặc host của Server cụm K8s (để trống nếu chạy local kubectl)
K8S_SSH_USER="${K8S_SSH_USER:-$(whoami)}"
K8S_SSH_PORT="${K8S_SSH_PORT:-22}"
K8S_SSH_KEY="${K8S_SSH_KEY:-}"
K8S_USE_SUDO="${K8S_USE_SUDO:-true}"  # Chạy 'sudo kubectl' trên server cụm

POD_NAME="${K8S_POD:-}"
NAMESPACE="${K8S_NAMESPACE:-default}"
CONTAINER_NAME="${K8S_CONTAINER:-}"
K8S_SELECTOR="${K8S_SELECTOR:-}"
KUBECONFIG_PATH="${KUBECONFIG:-${K8S_KUBECONFIG:-}}"
K8S_CONTEXT="${K8S_CONTEXT:-}"
K8S_MODE="${K8S_MODE:-debug}" # debug, sidecar, cp
SIDECAR_IMAGE="${SIDECAR_IMAGE:-docker.io/duyle95/profiling-sidecar:latest}"
SIDECAR_CONTAINER="${SIDECAR_CONTAINER:-sidecar}"
DEBUG_CONTAINER_NAME="capsizer-debugger"
PORT="${TARGET_PORT:-8000}"
TARGET_PID=""
PROFILE_TYPE="${PROFILE_TYPE:-pyspy}" # pyspy hoặc perf
OUT_DIR="${PROJECT_ROOT}/benchmark-results/$(date +%Y%m%d_%H%M%S)"

EXTRA_OPTS=()
ONLY_CHECK=false

# ==============================================================================
# HÀM NẠP CẤU HÌNH KỊCH BẢN ĐỘC LẬP (SCENARIO CONFIG)
# ==============================================================================
SCENARIO_ENV=""

load_scenario_env() {
  local target_env="$1"
  if [[ -f "$target_env" ]]; then
    local env_dir
    env_dir="$(cd "$(dirname "$target_env")" && pwd)"
    echo "==> [Cấu hình Kịch bản] Đang nạp từ: $target_env"
    # shellcheck disable=SC1090
    source "$target_env"

    if [[ -n "${TARGET_URL:-}" ]]; then URL="$TARGET_URL"; fi
    if [[ -n "${SCENARIO:-}" ]]; then SCENARIO="$SCENARIO"; fi
    if [[ -n "${METHOD:-}" ]]; then METHOD="$METHOD"; fi
    if [[ -n "${BODY:-}" ]]; then BODY="$BODY"; fi
    if [[ -n "${STEPS:-}" ]]; then STEPS="$STEPS"; fi
    if [[ -n "${STEP_SECONDS:-}" ]]; then STEP_SECONDS="$STEP_SECONDS"; fi
    if [[ -n "${BASELINE_WAIT:-}" ]]; then BASELINE_WAIT="$BASELINE_WAIT"; fi
    if [[ -n "${COOLDOWN:-}" ]]; then COOLDOWN="$COOLDOWN"; fi
    if [[ -n "${SETTLE:-}" ]]; then SETTLE="$SETTLE"; fi
    if [[ -n "${TIMEOUT:-}" ]]; then TIMEOUT="$TIMEOUT"; fi
    if [[ -n "${MAX_INFLIGHT:-}" ]]; then MAX_INFLIGHT="$MAX_INFLIGHT"; fi
    if [[ -n "${CACHE_BUST:-}" ]]; then CACHE_BUST="$CACHE_BUST"; fi
    if [[ -n "${PROFILE_TYPE:-}" ]]; then PROFILE_TYPE="$PROFILE_TYPE"; fi
    if [[ -n "${TARGET_PORT:-}" ]]; then PORT="$TARGET_PORT"; fi
    if [[ -n "${K8S_SELECTOR:-}" && -z "${POD_NAME:-}" ]]; then K8S_SELECTOR="$K8S_SELECTOR"; fi

    # Tự động chuyển file path tương đối thành tuyệt đối dựa vào thư mục kịch bản
    if [[ -n "${QUERIES_FILE:-}" && ! "$QUERIES_FILE" = /* && -f "$env_dir/$QUERIES_FILE" ]]; then
      QUERIES_FILE="$env_dir/$QUERIES_FILE"
    fi
    if [[ -n "${USERS_FILE:-}" && ! "$USERS_FILE" = /* && -f "$env_dir/$USERS_FILE" ]]; then
      USERS_FILE="$env_dir/$USERS_FILE"
    elif [[ -n "${USERS_CSV:-}" && ! "$USERS_CSV" = /* && -f "$env_dir/$USERS_CSV" ]]; then
      USERS_FILE="$env_dir/$USERS_CSV"
    fi
    if [[ -n "${BODY_FILE:-}" && ! "$BODY_FILE" = /* && -f "$env_dir/$BODY_FILE" ]]; then
      BODY_FILE="$env_dir/$BODY_FILE"
    fi
  fi
}

# Quét trước cờ --scenario-env hoặc --scenario để nạp cấu hình kịch bản trước khi ghi đè bằng cờ CLI
for ((i=1; i<=$#; i++)); do
  arg="${!i}"
  next_idx=$((i+1))
  next_arg="${!next_idx:-}"
  if [[ "$arg" == "--scenario-env" && -n "$next_arg" ]]; then
    SCENARIO_ENV="$next_arg"
  elif [[ "$arg" == "--scenario" && -n "$next_arg" && -z "$SCENARIO_ENV" ]]; then
    if [[ -f "${PROJECT_ROOT}/scenarios/${next_arg}/scenario.env" ]]; then
      SCENARIO_ENV="${PROJECT_ROOT}/scenarios/${next_arg}/scenario.env"
    elif [[ -f "${PROJECT_ROOT}/scenarios/${next_arg}/.env" ]]; then
      SCENARIO_ENV="${PROJECT_ROOT}/scenarios/${next_arg}/.env"
    fi
  fi
done

if [[ -n "$SCENARIO_ENV" ]]; then
  load_scenario_env "$SCENARIO_ENV"
fi

# Parse cờ dòng lệnh (ghi đè cấu hình)
while [[ $# -gt 0 ]]; do
  case $1 in
    --scenario-env) SCENARIO_ENV="$2"; shift 2 ;;
    --url) URL="$2"; shift 2 ;;
    --scenario)
      if [[ -f "${PROJECT_ROOT}/scenarios/$2/scenario.env" || -f "${PROJECT_ROOT}/scenarios/$2/.env" ]]; then
        :
      else
        SCENARIO="$2"
      fi
      shift 2 ;;
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
    --selector) K8S_SELECTOR="$2"; shift 2 ;;
    --kubeconfig) KUBECONFIG_PATH="$2"; shift 2 ;;
    --context) K8S_CONTEXT="$2"; shift 2 ;;
    --k8s-mode) K8S_MODE="$2"; shift 2 ;;
    --sidecar-image) SIDECAR_IMAGE="$2"; shift 2 ;;
    --sidecar-container) SIDECAR_CONTAINER="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --pid) TARGET_PID="$2"; shift 2 ;;
    --out-dir) OUT_DIR="$2"; shift 2 ;;

    # Máy phát tải
    --loadgen-mode) LOADGEN_MODE="$2"; shift 2 ;;
    --loadgen-host) LOADGEN_HOST="$2"; shift 2 ;;
    --loadgen-user) LOADGEN_USER="$2"; shift 2 ;;
    --loadgen-port) LOADGEN_PORT="$2"; shift 2 ;;
    --loadgen-key) LOADGEN_KEY="$2"; shift 2 ;;
    --loadgen-dir) LOADGEN_DIR="$2"; shift 2 ;;

    # Server Cụm K8s qua SSH
    --k8s-ssh-host) K8S_SSH_HOST="$2"; shift 2 ;;
    --k8s-ssh-user) K8S_SSH_USER="$2"; shift 2 ;;
    --k8s-ssh-port) K8S_SSH_PORT="$2"; shift 2 ;;
    --k8s-ssh-key) K8S_SSH_KEY="$2"; shift 2 ;;
    --k8s-no-sudo) K8S_USE_SUDO="false"; shift 1 ;;

    --env-file)
      ENV_FILE="$2"
      if [[ -f "$ENV_FILE" ]]; then
        # shellcheck disable=SC1090
        source "$ENV_FILE"
      fi
      shift 2 ;;
    --check|check)
      ONLY_CHECK=true
      shift 1 ;;
    -h|--help)
      echo "Sử dụng: $0 [OPTIONS]"
      echo ""
      echo "Tùy chọn Kịch bản (Scenario Packages):"
      echo "  --scenario <name>           Tên kịch bản (tự động nạp scenarios/<name>/scenario.env nếu có)"
      echo "  --scenario-env <file>       Đường dẫn file cấu hình kịch bản độc lập (scenario.env)"
      echo ""
      echo "Tùy chọn Kiểm thử Tải & Kịch bản (Ghi đè):"
      echo "  --url <url>                 Endpoint nhận request từ máy phát tải (default: $URL)"
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
      echo "  --opt <key=val>             Tùy chọn riêng cho Scenario (dùng nhiều lần)"
      echo "  --cache-bust                Thêm tham số timestamp ngẫu nhiên chống cache"
      echo "  --skip-verify               Bỏ qua xác thực TLS/SSL"
      echo "  --profile-type <type>       Công cụ profiling: pyspy hoặc perf (default: $PROFILE_TYPE)"
      echo "  --out-dir <dir>             Thư mục lưu trữ artifact kết quả"
      echo ""
      echo "Tùy chọn Máy Phát Tải (Machine 2: Load Generator):"
      echo "  --loadgen-mode <mode>       'local' (chính máy này) hoặc 'remote' (qua SSH) (default: $LOADGEN_MODE)"
      echo "  --loadgen-host <host>       IP/Host máy phát tải từ xa (khi mode=remote)"
      echo "  --loadgen-user <user>       SSH User (default: $LOADGEN_USER)"
      echo "  --loadgen-port <port>       SSH Port (default: $LOADGEN_PORT)"
      echo "  --loadgen-key <file>        Đường dẫn SSH private key"
      echo "  --loadgen-dir <dir>         Thư mục làm việc trên máy bắn tải (default: $LOADGEN_DIR)"
      echo ""
      echo "Tùy chọn Server Cụm Kubernetes (Machine 3: K8s Master/Bastion qua SSH):"
      echo "  --k8s-ssh-host <host>       IP/Host của Server cụm K8s (SSH passwordless)"
      echo "  --k8s-ssh-user <user>       SSH User vào cụm K8s (default: $K8S_SSH_USER)"
      echo "  --k8s-ssh-port <port>       SSH Port vào cụm K8s (default: $K8S_SSH_PORT)"
      echo "  --k8s-ssh-key <file>        Đường dẫn SSH private key vào cụm K8s"
      echo "  --k8s-no-sudo               Không dùng sudo khi gọi kubectl trên server cụm"
      echo "  --kubeconfig <file>         Đường dẫn file kubeconfig (trên server hoặc local)"
      echo "  --context <ctx>             Context trong kubeconfig"
      echo "  --namespace <ns>            K8s Namespace (default: $NAMESPACE)"
      echo "  --pod <pod_name>            Tên Pod mục tiêu trên K8s"
      echo "  --selector <label>          Label selector để tự động tìm Pod (vd: app=chatbot)"
      echo "  --container <name>          Tên container chứa app trong Pod (để debug target)"
      echo "  --k8s-mode <mode>           'debug' (gắn Ephemeral container), 'sidecar', hoặc 'cp' (default: $K8S_MODE)"
      echo "  --sidecar-image <image>     Image profiling sidecar (default: $SIDECAR_IMAGE)"
      echo "  --sidecar-container <name>  Tên container sidecar có sẵn (default: $SIDECAR_CONTAINER)"
      echo "  --port <port>               Cổng TCP của ứng dụng (default: $PORT)"
      echo ""
      echo "Kiểm tra kết nối:"
      echo "  --check                     Kiểm tra kết nối 3 máy (Skill -> LoadGen -> K8s) rồi thoát"
      exit 0
      ;;
    *)
      echo "Tham số không hợp lệ: $1" >&2
      exit 1
      ;;
  esac
done

# ==============================================================================
# HÀM KẾT NỐI VÀ ĐIỀU PHỐI SSH SERVER CỤM K8S (MACHINE 3)
# ==============================================================================
K8S_SSH_OPTS=(-p "$K8S_SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
if [[ -n "$K8S_SSH_KEY" ]]; then
  K8S_SSH_OPTS+=(-i "$K8S_SSH_KEY")
fi

run_k8s_ssh() {
  ssh "${K8S_SSH_OPTS[@]}" "${K8S_SSH_USER}@${K8S_SSH_HOST}" "$@"
}

run_k8s_scp_to() {
  local src="$1"
  local dst="$2"
  scp "${K8S_SSH_OPTS[@]}" "$src" "${K8S_SSH_USER}@${K8S_SSH_HOST}:$dst"
}

run_k8s_scp_from() {
  local src="$1"
  local dst="$2"
  scp "${K8S_SSH_OPTS[@]}" "${K8S_SSH_USER}@${K8S_SSH_HOST}:$src" "$dst"
}

# ==============================================================================
# HÀM THỰC THI KUBECTL (HỖ TRỢ CẢ SSH VÀO SERVER CỤM HOẶC CỤC BỘ)
# ==============================================================================
run_kubectl() {
  local sudo_prefix=""
  if [[ "$K8S_USE_SUDO" == "true" ]]; then
    sudo_prefix="sudo "
  fi

  local base_cmd="${sudo_prefix}kubectl"
  if [[ -n "$KUBECONFIG_PATH" ]]; then
    base_cmd+=" --kubeconfig='$KUBECONFIG_PATH'"
  fi
  if [[ -n "$K8S_CONTEXT" ]]; then
    base_cmd+=" --context='$K8S_CONTEXT'"
  fi

  if [[ -n "$K8S_SSH_HOST" ]]; then
    # Thực thi qua SSH trên Server cụm K8s
    run_k8s_ssh "${base_cmd} $*"
  else
    # Thực thi cục bộ
    eval "${base_cmd} $*"
  fi
}

resolve_target_pod() {
  if [[ -z "$POD_NAME" && -n "$K8S_SELECTOR" ]]; then
    echo "==> Đang tự động tìm Pod trong namespace '$NAMESPACE' theo selector '$K8S_SELECTOR'..."
    local found_pod
    found_pod=$(run_kubectl get pods -n "$NAMESPACE" -l "$K8S_SELECTOR" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -z "$found_pod" ]]; then
      found_pod=$(run_kubectl get pods -n "$NAMESPACE" -l "$K8S_SELECTOR" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    fi
    if [[ -n "$found_pod" ]]; then
      POD_NAME="$found_pod"
      echo "    ✓ Đã phát hiện Pod: $POD_NAME"
    else
      echo "[ERROR] Không tìm thấy Pod nào khớp với selector '$K8S_SELECTOR' trong namespace '$NAMESPACE'." >&2
      return 1
    fi
  fi
  return 0
}

# ==============================================================================
# HÀM KẾT NỐI VÀ ĐIỀU PHỐI MÁY PHÁT TẢI (MACHINE 2: LOAD GENERATOR)
# ==============================================================================
LOADGEN_SSH_OPTS=(-p "$LOADGEN_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
if [[ -n "$LOADGEN_KEY" ]]; then
  LOADGEN_SSH_OPTS+=(-i "$LOADGEN_KEY")
fi

run_loadgen_ssh() {
  ssh "${LOADGEN_SSH_OPTS[@]}" "${LOADGEN_USER}@${LOADGEN_HOST}" "$@"
}

run_loadgen_scp_to() {
  local src="$1"
  local dst="$2"
  scp "${LOADGEN_SSH_OPTS[@]}" "$src" "${LOADGEN_USER}@${LOADGEN_HOST}:$dst"
}

run_loadgen_scp_from() {
  local src="$1"
  local dst="$2"
  scp "${LOADGEN_SSH_OPTS[@]}" "${LOADGEN_USER}@${LOADGEN_HOST}:$src" "$dst"
}

# ==============================================================================
# HÀM KIỂM TRA TOÀN DIỆN KẾT NỐI 3 MÁY (PREFLIGHT HEALTH CHECK)
# ==============================================================================
preflight_check() {
  echo "================================================================================"
  echo "         KIỂM TRA LIÊN KẾT 3 MÁY (PREFLIGHT HEALTH CHECK)"
  echo "================================================================================"

  # [1/3] Máy chạy Skill (Orchestrator Host)
  echo "[1/3] Máy chạy Skill (Orchestrator - Local):"
  if command -v go >/dev/null 2>&1; then
    echo "      ✓ Golang: $(go version)"
  else
    echo "      [CẢNH BÁO] Không tìm thấy Go trên máy chạy Skill!"
  fi
  if [[ -z "$K8S_SSH_HOST" ]] && ! command -v kubectl >/dev/null 2>&1; then
    echo "      [LỖI] Không có kubectl cục bộ và chưa cấu hình --k8s-ssh-host!" >&2
    return 1
  fi
  echo "      ✓ Trạng thái: Sẵn sàng điều phối"

  # [2/3] Máy phát tải (Load Generator Host)
  echo ""
  echo "[2/3] Máy phát tải (Load Generator Host):"
  if [[ "$LOADGEN_MODE" == "remote" ]]; then
    if [[ -z "$LOADGEN_HOST" ]]; then
      echo "      [LỖI] Chưa cấu hình --loadgen-host hoặc REMOTE_HOST!" >&2
      return 1
    fi
    echo "      Đang kết nối SSH tới ${LOADGEN_USER}@${LOADGEN_HOST}:${LOADGEN_PORT}..."
    if run_loadgen_ssh "uname -srm" >/dev/null 2>&1; then
      local r_os
      r_os=$(run_loadgen_ssh "uname -srm")
      echo "      ✓ Kết nối SSH máy phát tải thành công ($r_os)"
    else
      echo "      [LỖI] Không thể SSH vào máy phát tải ${LOADGEN_USER}@${LOADGEN_HOST}!" >&2
      return 1
    fi

    # Kiểm tra luồng mạng từ máy bắn tải tới endpoint K8s
    echo "      Kiểm tra thông mạng từ máy phát tải tới target URL ($URL)..."
    local net_check
    net_check=$(run_loadgen_ssh "curl -s -k -I --connect-timeout 5 '$URL' | head -n 1" 2>/dev/null || true)
    if [[ -n "$net_check" ]]; then
      echo "      ✓ Máy phát tải kết nối được tới Target URL ($net_check)"
    else
      echo "      [CẢNH BÁO] Máy phát tải KHÔNG nhận được phản hồi HTTP từ '$URL'."
      echo "                 Hãy kiểm tra lại Ingress, Security Group, hoặc VPN giữa máy phát tải và K8s."
    fi
  else
    echo "      ✓ Chế độ phát tải cục bộ (Local Host)."
  fi

  # [3/3] Server Cụm Kubernetes từ xa
  echo ""
  echo "[3/3] Cụm Kubernetes từ xa (Remote K8s Target):"
  if [[ -n "$K8S_SSH_HOST" ]]; then
    echo "      Chế độ truy cập K8s: SSH vào Server cụm (${K8S_SSH_USER}@${K8S_SSH_HOST}:${K8S_SSH_PORT})"
    if run_k8s_ssh "uname -srm" >/dev/null 2>&1; then
      local k8s_srv_os
      k8s_srv_os=$(run_k8s_ssh "uname -srm")
      echo "      ✓ SSH vào Server cụm K8s thành công ($k8s_srv_os)"
    else
      echo "      [LỖI] Không thể kết nối SSH vào Server cụm K8s ${K8S_SSH_USER}@${K8S_SSH_HOST}!" >&2
      return 1
    fi

    # Kiểm tra quyền sudo root không cần mật khẩu
    if [[ "$K8S_USE_SUDO" == "true" ]]; then
      local sudo_user
      sudo_user=$(run_k8s_ssh "sudo whoami" 2>/dev/null || true)
      if [[ "$sudo_user" == "root" ]]; then
        echo "      ✓ Quyền sudo root không cần mật khẩu: Sẵn sàng (whoami: root)"
      else
        echo "      [CẢNH BÁO] Kiểm tra 'sudo whoami' trả về: '$sudo_user' (có thể cần nhập mật khẩu hoặc cấu hình NOPASSWD)"
      fi
    fi
  else
    echo "      Chế độ truy cập K8s: Kubectl trực tiếp từ máy cục bộ"
  fi

  echo "      Kiểm tra kết nối tới K8s API Server qua kubectl..."
  if run_kubectl cluster-info >/dev/null 2>&1; then
    local cluster_ep
    cluster_ep=$(run_kubectl cluster-info 2>/dev/null | head -n 1 | awk '{print $NF}' || true)
    echo "      ✓ Kết nối K8s API Server thành công ($cluster_ep)"
  else
    echo "      [LỖI] Không thể kết nối tới cụm Kubernetes qua kubectl!" >&2
    return 1
  fi

  resolve_target_pod || return 1

  if [[ -n "$POD_NAME" ]]; then
    echo "      Kiểm tra trạng thái Pod ${NAMESPACE}/${POD_NAME}..."
    local pod_status
    pod_status=$(run_kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "$pod_status" == "Running" ]]; then
      echo "      ✓ Pod mục tiêu đang hoạt động (Status: $pod_status)"
    else
      echo "      [CẢNH BÁO] Pod ${NAMESPACE}/${POD_NAME} có trạng thái: '$pod_status' (chưa phải Running)."
    fi
  fi

  echo "================================================================================"
  echo "✓ TẤT CẢ LIÊN KẾT 3 MÁY ĐỀU SẴN SÀNG!"
  echo "================================================================================"
  return 0
}

# Nếu người dùng chỉ muốn kiểm tra kết nối
if [[ "$ONLY_CHECK" == "true" ]]; then
  preflight_check
  exit 0
fi

mkdir -p "$OUT_DIR"

RAMP_LOG="$OUT_DIR/phase1-ramp.jsonl"
PROBE_LOG="$OUT_DIR/phase1-probe.jsonl"
REPORT_LOG="$OUT_DIR/phase1-report.txt"

# Tự động resolve Pod nếu chỉ có selector
resolve_target_pod || true

echo "================================================================================"
echo "    CAPSIZER 2-PHASE CAPACITY SIZING & PROFILING AUTOMATION"
echo "================================================================================"
echo "[1] Máy chạy Skill    : $(hostname) (Orchestrator Host)"
if [[ "$LOADGEN_MODE" == "remote" ]]; then
  echo "[2] Máy phát tải      : REMOTE (Host: ${LOADGEN_USER}@${LOADGEN_HOST}:${LOADGEN_PORT})"
else
  echo "[2] Máy phát tải      : LOCAL (Chính máy này)"
fi
if [[ -n "$K8S_SSH_HOST" ]]; then
  echo "[3] Server cụm K8s    : SSH ${K8S_SSH_USER}@${K8S_SSH_HOST}:${K8S_SSH_PORT} -> Pod ${NAMESPACE}/${POD_NAME}"
elif [[ -n "$POD_NAME" ]]; then
  echo "[3] Cụm K8s mục tiêu  : Local Kubectl -> Pod ${NAMESPACE}/${POD_NAME} (Mode: $K8S_MODE)"
else
  echo "[3] Ứng dụng mục tiêu : Local Host / External URL"
fi
echo "Endpoint nhận request : $URL (Scenario: $SCENARIO)"
echo "Dải bậc tải           : $STEPS (Mỗi bậc: ${STEP_SECONDS}s, Cooldown: ${COOLDOWN}s)"
echo "Thư mục output        : $OUT_DIR"
echo "================================================================================"

# ==============================================================================
# HÀM THIẾT LẬP MÁY BẮN TẢI TỪ XA
# ==============================================================================
setup_remote_loadgen() {
  if [[ -z "$LOADGEN_HOST" ]]; then
    echo "[ERROR] Chế độ phát tải remote yêu cầu --loadgen-host hoặc REMOTE_HOST trong .env.remote!" >&2
    exit 1
  fi

  echo "==> [Máy phát tải] Kiểm tra kết nối SSH tới ${LOADGEN_USER}@${LOADGEN_HOST}..."
  if ! run_loadgen_ssh "uname -srm" >/dev/null 2>&1; then
    echo "[ERROR] Không thể kết nối SSH tới ${LOADGEN_USER}@${LOADGEN_HOST}. Vui lòng kiểm tra lại SSH key và kết nối mạng." >&2
    exit 1
  fi
  local remote_os
  remote_os=$(run_loadgen_ssh "uname -srm")
  echo "    ✓ Kết nối SSH thành công ($remote_os)."

  local bin_dir="${PROJECT_ROOT}/bin"
  mkdir -p "$bin_dir"
  local local_ramp_bin="${bin_dir}/capsizer-ramp-linux-amd64"

  echo "==> [Máy phát tải] Biên dịch tĩnh cmd/ramp (Linux amd64)..."
  (
    cd "${PROJECT_ROOT}"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$local_ramp_bin" ./cmd/ramp
  )

  echo "==> [Máy phát tải] Đồng bộ binary và cấu hình lên ${LOADGEN_DIR}..."
  run_loadgen_ssh "mkdir -p '${LOADGEN_DIR}'"
  run_loadgen_scp_to "$local_ramp_bin" "${LOADGEN_DIR}/capsizer-ramp"
  run_loadgen_ssh "chmod +x '${LOADGEN_DIR}/capsizer-ramp'"

  if [[ -n "$QUERIES_FILE" && -f "$QUERIES_FILE" ]]; then
    run_loadgen_scp_to "$QUERIES_FILE" "${LOADGEN_DIR}/queries.txt"
  fi
  if [[ -n "$USERS_FILE" && -f "$USERS_FILE" ]]; then
    run_loadgen_scp_to "$USERS_FILE" "${LOADGEN_DIR}/users.csv"
  fi
  if [[ -n "$BODY_FILE" && -f "$BODY_FILE" ]]; then
    run_loadgen_scp_to "$BODY_FILE" "${LOADGEN_DIR}/body_payload"
  fi
  echo "    ✓ Chuẩn bị máy phát tải hoàn tất."
}

# Hàm thực thi phát tải (trừu tượng hóa local và remote)
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
      run_loadgen_ssh "cd '${LOADGEN_DIR}' && ${r_cmd}" > "$OUT_DIR/remote-loadgen.log" 2>&1 &
      LOADGEN_BG_PID=$!
    else
      run_loadgen_ssh "cd '${LOADGEN_DIR}' && ${r_cmd}"
      run_loadgen_scp_from "${LOADGEN_DIR}/current-ramp.jsonl" "$l_out_file"
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
# HÀM THỰC THI TRONG CỤM KUBERNETES TỪ XA
# ==============================================================================
setup_k8s_debug_container() {
  echo "==> Đang kiểm tra / gắn Ephemeral Debug Container ($DEBUG_CONTAINER_NAME) vào Pod $POD_NAME..."
  local TARGET_FLAG=""
  if [[ -n "$CONTAINER_NAME" ]]; then
    TARGET_FLAG="--target=$CONTAINER_NAME"
  fi

  local HAS_DEBUG
  HAS_DEBUG=$(run_kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath="{.spec.ephemeralContainers[*].name}" 2>/dev/null || true)
  if echo "$HAS_DEBUG" | grep -qw "$DEBUG_CONTAINER_NAME"; then
    echo "    ✓ Ephemeral container $DEBUG_CONTAINER_NAME đã sẵn sàng trong Pod."
  else
    echo "    Đang đính kèm container debug với quyền SYS_PTRACE..."
    run_kubectl debug "$POD_NAME" -n "$NAMESPACE" \
      --image="$SIDECAR_IMAGE" \
      --custom='{"spec":{"ephemeralContainers":[{"name":"'"$DEBUG_CONTAINER_NAME"'","image":"'"$SIDECAR_IMAGE"'","securityContext":{"capabilities":{"add":["SYS_PTRACE"]},"runAsUser":0},"stdin":true,"tty":true}]}}' \
      $TARGET_FLAG \
      --profile=sysadmin 2>/dev/null || \
    run_kubectl debug "$POD_NAME" -n "$NAMESPACE" \
      --image="$SIDECAR_IMAGE" \
      $TARGET_FLAG \
      --profile=sysadmin 2>/dev/null || \
    run_kubectl debug "$POD_NAME" -n "$NAMESPACE" \
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
    run_kubectl exec -n "$NAMESPACE" "$POD_NAME" -c "$DEBUG_CONTAINER_NAME" -- sh -c "$CMD"
  elif [[ "$K8S_MODE" == "sidecar" ]]; then
    run_kubectl exec -n "$NAMESPACE" "$POD_NAME" -c "$SIDECAR_CONTAINER" -- sh -c "$CMD"
  elif [[ "$K8S_MODE" == "cp" ]]; then
    local C_FLAG=""
    if [[ -n "$CONTAINER_NAME" ]]; then
      C_FLAG="-c $CONTAINER_NAME"
    fi
    run_kubectl exec -n "$NAMESPACE" "$POD_NAME" $C_FLAG -- sh -c "$CMD"
  fi
}

copy_from_target() {
  local REMOTE_PATH="$1"
  local LOCAL_PATH="$2"

  if [[ -z "$POD_NAME" ]]; then
    cp "$REMOTE_PATH" "$LOCAL_PATH" 2>/dev/null || true
    return
  fi

  local C_FLAG=""
  if [[ "$K8S_MODE" == "debug" ]]; then
    C_FLAG="-c $DEBUG_CONTAINER_NAME"
  elif [[ "$K8S_MODE" == "sidecar" ]]; then
    C_FLAG="-c $SIDECAR_CONTAINER"
  elif [[ -n "$CONTAINER_NAME" ]]; then
    C_FLAG="-c $CONTAINER_NAME"
  fi

  if [[ -n "$K8S_SSH_HOST" ]]; then
    # Kéo từ Pod ra K8s server host -> sau đó scp về Skill machine
    local srv_tmp="/tmp/_capsizer_xfer_$$.tmp"
    run_kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" "$srv_tmp" $C_FLAG 2>/dev/null || true
    run_k8s_ssh "sudo chmod 644 '$srv_tmp' 2>/dev/null || true"
    run_k8s_scp_from "$srv_tmp" "$LOCAL_PATH" 2>/dev/null || true
    run_k8s_ssh "sudo rm -f '$srv_tmp' 2>/dev/null || true"
  else
    # Kubectl trực tiếp từ Skill machine
    run_kubectl cp "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" "$LOCAL_PATH" $C_FLAG 2>/dev/null || true
  fi
}

copy_to_target() {
  local LOCAL_PATH="$1"
  local REMOTE_PATH="$2"

  if [[ -z "$POD_NAME" ]]; then
    cp "$LOCAL_PATH" "$REMOTE_PATH" 2>/dev/null || true
    return
  fi

  local C_FLAG=""
  if [[ -n "$CONTAINER_NAME" ]]; then
    C_FLAG="-c $CONTAINER_NAME"
  fi

  if [[ -n "$K8S_SSH_HOST" ]]; then
    # Đẩy từ Skill machine lên K8s server host -> sau đó kubectl cp vào Pod
    local srv_tmp="/tmp/_capsizer_probe_$$.tmp"
    run_k8s_scp_to "$LOCAL_PATH" "$srv_tmp" 2>/dev/null || true
    run_kubectl cp "$srv_tmp" "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" $C_FLAG 2>/dev/null || true
    run_k8s_ssh "sudo rm -f '$srv_tmp' 2>/dev/null || true"
  else
    # Kubectl trực tiếp từ Skill machine
    run_kubectl cp "$LOCAL_PATH" "${NAMESPACE}/${POD_NAME}:${REMOTE_PATH}" $C_FLAG 2>/dev/null || true
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
    echo "==> Đang biên dịch tĩnh và copy binary probe vào Pod..."
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o "$OUT_DIR/probe" ./cmd/probe
    local C_FLAG=""
    if [[ -n "$CONTAINER_NAME" ]]; then C_FLAG="-c $CONTAINER_NAME"; fi
    copy_to_target "$OUT_DIR/probe" "/tmp/probe"
    run_kubectl exec -n "$NAMESPACE" "$POD_NAME" $C_FLAG -- sh -c "nohup /tmp/probe -port $PORT -out /tmp/probe.jsonl >/tmp/probe.log 2>&1 &"
  fi
else
  echo "==> Đang khởi chạy probe trên máy local..."
  mkdir -p "$PROJECT_ROOT/bin"
  if [[ ! -f "$PROJECT_ROOT/bin/probe" ]]; then
    (cd "$PROJECT_ROOT" && go build -ldflags="-s -w" -o "$PROJECT_ROOT/bin/probe" ./cmd/probe)
  fi
  "$PROJECT_ROOT/bin/probe" -port "$PORT" -out "$PROBE_LOG" &
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
  run_loadgen_scp_from "${LOADGEN_DIR}/current-ramp.jsonl" "$PHASE2_RAMP_LOG" 2>/dev/null || true
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
