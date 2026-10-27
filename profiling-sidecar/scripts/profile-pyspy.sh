#!/usr/bin/env bash
# ==============================================================================
# profile-pyspy.sh
# Tiện ích thu thập CPU Profile và tạo FlameGraph cho Python bằng py-spy
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
  Tự động gắn vào tiến trình Python, lấy mẫu call stack và xuất biểu đồ FlameGraph SVG.

Tùy chọn:
  -p, --pid PID          PID của tiến trình Python mục tiêu.
                         (Nếu bỏ trống, script sẽ tự quét và hiển thị tiến trình Python)
  -d, --duration SEC     Thời gian lấy mẫu tính bằng giây (Mặc định: 30)
  -r, --rate HZ          Tần số lấy mẫu (Mặc định: 100 Hz)
  -o, --output FILE      Đường dẫn file SVG đầu ra (Mặc định: /tmp/flamegraph-python-<PID>-<TIME>.svg)
  -n, --native           Thu thập cả call stack của C/C++ native extensions
  -s, --subprocesses     Thu thập cả các tiến trình con (subprocesses)
  --top                  Chạy chế độ live top view (tương tự 'top' cho code Python)
  --dump                 In call stack hiện tại ra màn hình (dump) rồi thoát
  -h, --help             Hiển thị trợ giúp này

Ví dụ:
  $(basename "$0")                         # Tự động tìm tiến trình Python và profile 30s
  $(basename "$0") 1234                    # Profile PID 1234 trong 30s
  $(basename "$0") -p 1234 -d 60 --native  # Profile 60s kèm C-extensions
  $(basename "$0") -p 1234 --top           # Xem live CPU top theo code
EOF
    exit "$EXIT_CODE"
}

# Xử lý --help trước
for arg in "$@"; do
    if [[ "$arg" == "-h" || "$arg" == "--help" ]]; then
        usage 0
    fi
done

# Kiểm tra công cụ py-spy
if ! command -v py-spy &>/dev/null; then
    log_error "Không tìm thấy lệnh 'py-spy' trong container."
    exit 1
fi

PID=""
DURATION=30
RATE=100
OUTPUT=""
NATIVE_FLAG=""
SUBPROC_FLAG=""
MODE="record"

# Xử lý tham số dòng lệnh
while [[ $# -gt 0 ]]; do
    case "$1" in
        -p|--pid)
            PID="$2"
            shift 2
            ;;
        -d|--duration)
            DURATION="$2"
            shift 2
            ;;
        -r|--rate)
            RATE="$2"
            shift 2
            ;;
        -o|--output)
            OUTPUT="$2"
            shift 2
            ;;
        -n|--native)
            NATIVE_FLAG="--native"
            shift
            ;;
        -s|--subprocesses)
            SUBPROC_FLAG="--subprocesses"
            shift
            ;;
        --top)
            MODE="top"
            shift
            ;;
        --dump)
            MODE="dump"
            shift
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

# Tự động tìm PID nếu chưa được chỉ định
if [[ -z "$PID" ]]; then
    log_info "Đang tìm kiếm các tiến trình Python đang chạy..."
    log_cmd "ps -eo pid,comm,args"
    
    # Tìm các tiến trình python/gunicorn/uvicorn/celery
    PYTHON_PROCS=$(ps -eo pid,comm,args 2>/dev/null | grep -iE 'python|gunicorn|uvicorn|celery' | grep -v grep | grep -v "$(basename "$0")" || true)

    if [[ -z "$PYTHON_PROCS" ]]; then
        log_error "Không tìm thấy tiến trình Python nào trong namespace này."
        log_warn "Danh sách toàn bộ tiến trình hiện có:"
        ps -eo pid,comm,args || ps aux
        exit 1
    fi

    PROC_COUNT=$(echo "$PYTHON_PROCS" | wc -l)
    if [[ "$PROC_COUNT" -eq 1 ]]; then
        PID=$(echo "$PYTHON_PROCS" | awk '{print $1}')
        CMD=$(echo "$PYTHON_PROCS" | awk '{$1=""; print $0}' | sed 's/^[ \t]*//')
        log_info "Tìm thấy duy nhất 1 tiến trình Python: PID ${CYAN}${PID}${NC} (${CMD})"
    else
        log_warn "Tìm thấy nhiều tiến trình Python. Vui lòng chọn PID:"
        echo "----------------------------------------------------------------------"
        printf "%-8s %s\n" "PID" "COMMAND"
        echo "----------------------------------------------------------------------"
        echo "$PYTHON_PROCS" | while read -r p_pid p_comm p_args; do
            printf "%-8s %s\n" "$p_pid" "$p_comm $p_args"
        done
        echo "----------------------------------------------------------------------"
        read -r -p "Nhập PID cần profile: " SELECTED_PID
        if [[ -z "$SELECTED_PID" || ! "$SELECTED_PID" =~ ^[0-9]+$ ]]; then
            log_error "PID không hợp lệ."
            exit 1
        fi
        PID="$SELECTED_PID"
    fi
fi

# Kiểm tra PID có tồn tại không
if ! kill -0 "$PID" 2>/dev/null; then
    log_error "Tiến trình PID $PID không tồn tại hoặc không thể truy cập."
    exit 1
fi

PROC_NAME=$(ps -p "$PID" -o comm= 2>/dev/null || echo "python")

# Chế độ TOP
if [[ "$MODE" == "top" ]]; then
    log_info "Khởi động py-spy top cho PID $PID ($PROC_NAME)... (Nhấn Ctrl+C để thoát)"
    log_cmd "py-spy top --pid $PID $NATIVE_FLAG $SUBPROC_FLAG"
    exec py-spy top --pid "$PID" $NATIVE_FLAG $SUBPROC_FLAG
fi

# Chế độ DUMP
if [[ "$MODE" == "dump" ]]; then
    log_info "Đang trích xuất Call Stack hiện tại của PID $PID ($PROC_NAME)..."
    log_cmd "py-spy dump --pid $PID $NATIVE_FLAG"
    exec py-spy dump --pid "$PID" $NATIVE_FLAG
fi

# Chế độ RECORD (Mặc định)
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
if [[ -z "$OUTPUT" ]]; then
    OUTPUT="/tmp/flamegraph-python-${PID}-${TIMESTAMP}.svg"
fi

echo -e "\n${CYAN}======================================================================${NC}"
echo -e "${CYAN}             THU THẬP FLAMEGRAPH CHO PYTHON (PY-SPY)                  ${NC}"
echo -e "${CYAN}======================================================================${NC}"
echo -e "  - Tiến trình đích   : PID ${GREEN}${PID}${NC} (${PROC_NAME})"
echo -e "  - Thời gian đo      : ${YELLOW}${DURATION}s${NC}"
echo -e "  - Tần số lấy mẫu    : ${RATE} Hz"
echo -e "  - File SVG đầu ra   : ${GREEN}${OUTPUT}${NC}"
[[ -n "$NATIVE_FLAG" ]] && echo -e "  - Kèm C-Extensions  : Có (--native)"
[[ -n "$SUBPROC_FLAG" ]] && echo -e "  - Subprocesses      : Có (--subprocesses)"
echo -e "${CYAN}======================================================================${NC}\n"

log_info "Bắt đầu thu thập dữ liệu (Vui lòng đợi ${DURATION} giây)..."
log_cmd "py-spy record --pid $PID --duration $DURATION --rate $RATE --output $OUTPUT $NATIVE_FLAG $SUBPROC_FLAG"

py-spy record \
    --pid "$PID" \
    --duration "$DURATION" \
    --rate "$RATE" \
    --output "$OUTPUT" \
    $NATIVE_FLAG \
    $SUBPROC_FLAG

if [[ -f "$OUTPUT" && -s "$OUTPUT" ]]; then
    FILE_SIZE=$(ls -lh "$OUTPUT" | awk '{print $5}')
    echo ""
    log_success "Đã tạo FlameGraph thành công! Kích thước: ${FILE_SIZE}"
    log_success "Đường dẫn file: ${OUTPUT}"
    echo ""
    echo -e "${YELLOW}👉 Lệnh tải file SVG về máy cá nhân (chạy trên máy local):${NC}"
    echo -e "   ${CYAN}kubectl cp <NAMESPACE>/<POD_NAME>:${OUTPUT} ./${OUTPUT##*/} -c <CONTAINER_NAME>${NC}\n"
else
    log_error "Quá trình tạo file FlameGraph thất bại hoặc file rỗng."
    exit 1
fi
