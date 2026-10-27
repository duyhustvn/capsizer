#!/usr/bin/env bash
# ==============================================================================
# profile-perf.sh
# Tiện ích thu thập CPU Profile và tạo FlameGraph bằng Linux perf & FlameGraph
# ==============================================================================

set -euo pipefail

# Màu sắc hiển thị
if [ -t 1 ]; then
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    RED='\033[0;31m'
    BLUE='\033[0;34m'
    CYAN='\033[0;36m'
    BOLD='\033[1m'
    NC='\033[0m'
else
    GREEN=''
    YELLOW=''
    RED=''
    BLUE=''
    CYAN=''
    BOLD=''
    NC=''
fi

log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_cmd() {
    local INDENT=""
    if [[ "$#" -gt 1 && "$1" =~ ^[[:space:]]+$ ]]; then
        INDENT="$1"
        shift
    fi
    echo -e "${INDENT}${CYAN}[CMD]${NC} ${BOLD}$*${NC}"
}

usage() {
    local EXIT_CODE="${1:-0}"
    cat <<EOF
Sử dụng: $(basename "$0") [OPTIONS] [PID]

Mô tả:
  Thu thập CPU stack trace bằng Linux perf và render biểu đồ FlameGraph SVG tương tác.

Tùy chọn:
  -p, --pid PID          PID của tiến trình mục tiêu cần profile
  -a, --all-cpus         Profile toàn bộ hệ thống (tất cả CPU trong namespace/node)
  -d, --duration SEC     Thời gian lấy mẫu tính bằng giây (Mặc định: 30)
  -F, --freq HZ          Tần số lấy mẫu Hz (Mặc định: 99 Hz - tránh lockstep sampling)
  -g, --call-graph TYPE  Phương pháp lấy call stack: 'fp' (frame pointer, mặc định) hoặc 'dwarf' (Go/Rust/C++)
  -e, --event EVENT      Sự kiện cần đo (Mặc định: cycles, các tùy chọn khác: cpu-clock, instructions, cache-misses)
  -o, --output FILE      Đường dẫn file SVG đầu ra (Mặc định: /tmp/flamegraph-perf-<TARGET>-<TIME>.svg)
  -k, --keep-data        Giữ lại file thô /tmp/perf.data để phân tích bằng 'perf report'
  --title TITLE          Tiêu đề hiển thị trên biểu đồ FlameGraph
  -h, --help             Hiển thị trợ giúp này

Ví dụ:
  $(basename "$0") 1234                          # Profile PID 1234 trong 30s
  $(basename "$0") -p 1234 -d 60 -g dwarf        # Profile 60s dùng DWARF (cho Go/Rust)
  $(basename "$0") -a -d 15                      # Profile toàn bộ hệ thống trong 15s
  $(basename "$0") -p 1234 -k                    # Profile và giữ lại file perf.data
EOF
    exit "$EXIT_CODE"
}

# Xử lý --help trước
for arg in "$@"; do
    if [[ "$arg" == "-h" || "$arg" == "--help" ]]; then
        usage 0
    fi
done

# Kiểm tra các công cụ cần thiết
for cmd in perf flamegraph.pl stackcollapse-perf.pl; do
    if ! command -v "$cmd" &>/dev/null; then
        log_error "Thiếu công cụ '$cmd'. Hãy đảm bảo PATH bao gồm /opt/FlameGraph."
        exit 1
    fi
done

PID=""
ALL_CPUS=0
DURATION=30
FREQ=99
CALL_GRAPH="fp"
EVENT="cycles"
OUTPUT=""
KEEP_DATA=0
TITLE=""

# Xử lý tham số dòng lệnh
while [[ $# -gt 0 ]]; do
    case "$1" in
        -p|--pid)
            PID="$2"
            shift 2
            ;;
        -a|--all-cpus)
            ALL_CPUS=1
            shift
            ;;
        -d|--duration)
            DURATION="$2"
            shift 2
            ;;
        -F|--freq)
            FREQ="$2"
            shift 2
            ;;
        -g|--call-graph)
            CALL_GRAPH="$2"
            shift 2
            ;;
        -e|--event)
            EVENT="$2"
            shift 2
            ;;
        -o|--output)
            OUTPUT="$2"
            shift 2
            ;;
        -k|--keep-data)
            KEEP_DATA=1
            shift
            ;;
        --title)
            TITLE="$2"
            shift 2
            ;;
        -h|--help)
            usage
            ;;
        *)
            if [[ -z "$PID" && "$1" =~ ^[0-9]+$ ]]; then
                PID="$1"
                shift
            else
                log_error "Tham số không hợp lệ: $1"
                usage
            fi
            ;;
    esac
done

# Kiểm tra mục tiêu profiling
if [[ "$ALL_CPUS" -eq 1 ]]; then
    TARGET_ARG="-a"
    TARGET_DESC="Toàn bộ CPU (System-wide)"
    TARGET_NAME="all-cpus"
elif [[ -n "$PID" ]]; then
    if ! kill -0 "$PID" 2>/dev/null; then
        log_error "Tiến trình PID $PID không tồn tại hoặc không thể truy cập."
        exit 1
    fi
    TARGET_ARG="-p $PID"
    PROC_NAME=$(ps -p "$PID" -o comm= 2>/dev/null || echo "unknown")
    TARGET_DESC="PID ${GREEN}${PID}${NC} (${PROC_NAME})"
    TARGET_NAME="pid-${PID}"
else
    log_warn "Chưa chỉ định PID và không dùng cờ -a (all-cpus)."
    log_info "Danh sách các tiến trình đang chạy trong namespace:"
    log_cmd "ps -eo pid,comm,args"
    echo "----------------------------------------------------------------------"
    ps -eo pid,comm,args || ps aux
    echo "----------------------------------------------------------------------"
    read -r -p "Nhập PID cần profile (hoặc gõ 'all' để profile tất cả CPU): " USER_INPUT
    if [[ "$USER_INPUT" == "all" || "$USER_INPUT" == "-a" ]]; then
        TARGET_ARG="-a"
        TARGET_DESC="Toàn bộ CPU (System-wide)"
        TARGET_NAME="all-cpus"
    elif [[ "$USER_INPUT" =~ ^[0-9]+$ ]]; then
        PID="$USER_INPUT"
        if ! kill -0 "$PID" 2>/dev/null; then
            log_error "PID $PID không tồn tại."
            exit 1
        fi
        TARGET_ARG="-p $PID"
        PROC_NAME=$(ps -p "$PID" -o comm= 2>/dev/null || echo "unknown")
        TARGET_DESC="PID ${GREEN}${PID}${NC} (${PROC_NAME})"
        TARGET_NAME="pid-${PID}"
    else
        log_error "Lựa chọn không hợp lệ."
        exit 1
    fi
fi

TIMESTAMP=$(date +%Y%m%d_%H%M%S)
TEMP_DATA="/tmp/perf-${TARGET_NAME}-${TIMESTAMP}.data"
if [[ -z "$OUTPUT" ]]; then
    OUTPUT="/tmp/flamegraph-perf-${TARGET_NAME}-${TIMESTAMP}.svg"
fi
if [[ -z "$TITLE" ]]; then
    TITLE="CPU FlameGraph (${TARGET_NAME}, ${DURATION}s, ${FREQ}Hz)"
fi

echo -e "\n${CYAN}======================================================================${NC}"
echo -e "${CYAN}             THU THẬP FLAMEGRAPH BẰNG LINUX PERF                      ${NC}"
echo -e "${CYAN}======================================================================${NC}"
echo -e "  - Mục tiêu đo       : ${TARGET_DESC}"
echo -e "  - Thời gian đo      : ${YELLOW}${DURATION}s${NC}"
echo -e "  - Tần số lấy mẫu    : ${FREQ} Hz"
echo -e "  - Sự kiện           : ${EVENT}"
echo -e "  - Call-graph mode   : ${CALL_GRAPH}"
echo -e "  - File SVG đầu ra   : ${GREEN}${OUTPUT}${NC}"
echo -e "${CYAN}======================================================================${NC}\n"

log_info "Bước 1/2: Đang ghi mẫu CPU bằng 'perf record' (Vui lòng đợi ${DURATION}s)..."
log_cmd "perf record -F $FREQ $TARGET_ARG -g --call-graph $CALL_GRAPH -e $EVENT -o $TEMP_DATA -- sleep $DURATION"

# Thực hiện perf record
# shellcheck disable=SC2086
if ! perf record -F "$FREQ" $TARGET_ARG -g --call-graph "$CALL_GRAPH" -e "$EVENT" -o "$TEMP_DATA" -- sleep "$DURATION"; then
    log_error "Lệnh 'perf record' thất bại."
    log_warn "Nguyên nhân có thể do thiếu quyền SYS_ADMIN hoặc SYS_PTRACE, hoặc kptr_restrict."
    rm -f "$TEMP_DATA"
    exit 1
fi

if [[ ! -s "$TEMP_DATA" ]]; then
    log_error "Không có dữ liệu mẫu nào được ghi lại (file perf.data rỗng)."
    rm -f "$TEMP_DATA"
    exit 1
fi

log_info "Bước 2/2: Đang chuyển đổi dữ liệu và dựng biểu đồ FlameGraph SVG..."
log_cmd "perf script -i $TEMP_DATA | stackcollapse-perf.pl | flamegraph.pl --title=\"$TITLE\" > $OUTPUT"

# Chuyển đổi: perf script | stackcollapse | flamegraph
if ! perf script -i "$TEMP_DATA" | stackcollapse-perf.pl | flamegraph.pl --title="$TITLE" > "$OUTPUT"; then
    log_error "Quá trình render FlameGraph thất bại."
    [[ "$KEEP_DATA" -eq 0 ]] && rm -f "$TEMP_DATA"
    exit 1
fi

# Dọn dẹp hoặc thông báo file perf.data
if [[ "$KEEP_DATA" -eq 1 ]]; then
    log_info "Đã giữ lại file dữ liệu thô: ${TEMP_DATA}"
    log_info "Để phân tích dữ liệu trực tiếp trên terminal, dùng lệnh:"
    log_cmd "perf report -i ${TEMP_DATA}"
else
    rm -f "$TEMP_DATA"
fi

if [[ -f "$OUTPUT" && -s "$OUTPUT" ]]; then
    FILE_SIZE=$(ls -lh "$OUTPUT" | awk '{print $5}')
    echo ""
    log_success "Đã tạo FlameGraph thành công! Kích thước: ${FILE_SIZE}"
    log_success "Đường dẫn file: ${OUTPUT}"
    echo ""
    echo -e "${YELLOW}👉 Lệnh tải file SVG về máy cá nhân (chạy trên máy local):${NC}"
    echo -e "   ${CYAN}kubectl cp <NAMESPACE>/<POD_NAME>:${OUTPUT} ./${OUTPUT##*/} -c <CONTAINER_NAME>${NC}\n"
else
    log_error "Quá trình tạo file FlameGraph thất bại."
    exit 1
fi
