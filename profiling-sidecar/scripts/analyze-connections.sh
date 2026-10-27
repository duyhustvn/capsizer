#!/usr/bin/env bash
# ==============================================================================
# analyze-connections.sh
# Tiện ích phân tích kết nối mạng và trạng thái socket trong Pod/Container bằng ss
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
log_warn()    { echo -e "${YELLOW}[CẢNH BÁO]${NC} $*"; }
log_error()   { echo -e "${RED}[LỖI]${NC} $*" >&2; }
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
Sử dụng: $(basename "$0") [OPTIONS]

Mô tả:
  Phân tích toàn diện trạng thái kết nối mạng (TCP/UDP, Listening, Queues, Top Remote IPs)
  giúp phát hiện lỗi rò rỉ kết nối, nghẽn socket queue, TIME-WAIT bùng nổ, timeout.

Tùy chọn:
  -w, --watch [SEC]      Chế độ theo dõi liên tục (refresh sau mỗi SEC giây, mặc định: 2s)
  -s, --state STATE      Lọc danh sách kết nối theo trạng thái TCP:
                         (estab, close-wait, time-wait, syn-sent, syn-recv, listen, fin-wait-1, fin-wait-2, v.v.)
  -p, --port PORT        Lọc kết nối theo cổng (Local hoặc Remote port)
  -q, --queues           Chỉ hiển thị các socket bị ứ đọng hàng đợi (Recv-Q > 0 hoặc Send-Q > 0)
  -t, --top N            Số lượng Top Remote IPs hiển thị (Mặc định: 10)
  -h, --help             Hiển thị trợ giúp này

Ví dụ:
  $(basename "$0")                       # Chạy báo cáo tổng quan toàn diện
  $(basename "$0") -w                    # Theo dõi kết nối liên tục (mỗi 2s)
  $(basename "$0") -s close-wait         # Chỉ liệt kê các kết nối đang bị kẹt ở CLOSE-WAIT
  $(basename "$0") -p 8080               # Lọc tất cả kết nối liên quan tới port 8080
  $(basename "$0") --queues              # Kiểm tra các kết nối bị nghẽn buffer/accept queue
EOF
    exit "$EXIT_CODE"
}

# Kiểm tra công cụ ss
if ! command -v ss &>/dev/null; then
    log_error "Không tìm thấy lệnh 'ss'. Vui lòng cài đặt iproute2."
    exit 1
fi

WATCH_MODE=0
WATCH_INTERVAL=2
FILTER_STATE=""
FILTER_PORT=""
FILTER_QUEUES=0
TOP_N=10

while [[ $# -gt 0 ]]; do
    case "$1" in
        -w|--watch)
            WATCH_MODE=1
            if [[ $# -gt 1 && "$2" =~ ^[0-9]+$ ]]; then
                WATCH_INTERVAL="$2"
                shift 2
            else
                shift
            fi
            ;;
        -s|--state)
            FILTER_STATE="${2,,}" # chuyển chữ thường
            shift 2
            ;;
        -p|--port)
            FILTER_PORT="$2"
            shift 2
            ;;
        -q|--queues)
            FILTER_QUEUES=1
            shift
            ;;
        -t|--top)
            TOP_N="$2"
            shift 2
            ;;
        -h|--help)
            usage 0
            ;;
        *)
            log_error "Tham số không hợp lệ: $1"
            usage 1
            ;;
    esac
done

# Hàm định dạng bảng hiển thị (ưu tiên dùng column nếu có, fallback sang cat nếu thiếu)
format_table() {
    if command -v column &>/dev/null; then
        column -t
    else
        cat
    fi
}

# Hàm xuất kết nối theo bộ lọc tùy chỉnh
run_filtered() {
    echo -e "${CYAN}=== DANH SÁCH KẾT NỐI THEO BỘ LỌC ===${NC}"
    local SS_ARGS="-t -a -n -p"

    if [[ -n "$FILTER_STATE" ]]; then
        SS_ARGS="$SS_ARGS state $FILTER_STATE"
    fi

    if [[ -n "$FILTER_PORT" ]]; then
        SS_ARGS="$SS_ARGS ( dport = :$FILTER_PORT or sport = :$FILTER_PORT )"
    fi

    if [[ "$FILTER_QUEUES" -eq 1 ]]; then
        echo -e "${YELLOW}Đang lọc các socket có Recv-Q > 0 hoặc Send-Q > 0...${NC}"
        log_cmd "ss $SS_ARGS | awk 'NR==1 || (\$2 > 0 || \$3 > 0)'"
        # shellcheck disable=SC2086
        ss $SS_ARGS | awk 'NR==1 || ($2 > 0 || $3 > 0)' | format_table
    else
        log_cmd "ss $SS_ARGS"
        # shellcheck disable=SC2086
        ss $SS_ARGS | format_table
    fi
}

# Báo cáo tổng quan chi tiết
generate_report() {
    local NOW
    NOW=$(date "+%Y-%m-%d %H:%M:%S")

    echo -e "${BOLD}${CYAN}======================================================================${NC}"
    echo -e "${BOLD}${CYAN}         BÁO CÁO PHÂN TÍCH KẾT NỐI MẠNG (SS NETWORK INSPECTOR)       ${NC}"
    echo -e "${CYAN}  Thời gian: ${NOW} | Namespace Network Pod                 ${NC}"
    echo -e "${BOLD}${CYAN}======================================================================${NC}\n"

    # 1. Tổng quan Socket từ ss -s
    echo -e "${BOLD}1. 📊 TỔNG QUAN SOCKET (Summary):${NC}"
    log_cmd "   " "ss -s"
    ss -s | sed 's/^/   /'
    echo ""

    # 2. Thống kê số lượng kết nối theo trạng thái TCP
    echo -e "${BOLD}2. 📈 PHÂN BỔ TRẠNG THÁI TCP (TCP States Breakdown):${NC}"
    log_cmd "   " "ss -tan"
    printf "   %-18s : %s\n" "TRẠNG THÁI" "SỐ LƯỢNG"
    echo "   ------------------------------------"

    local RAW_TCP
    RAW_TCP=$(ss -tan 2>/dev/null | awk 'NR>1 {print $1}')

    local COUNT_ESTAB COUNT_TIMEWAIT COUNT_CLOSEWAIT COUNT_LISTEN COUNT_SYNSENT COUNT_SYNRECV COUNT_FINWAIT1 COUNT_FINWAIT2 COUNT_LASTACK COUNT_TOTAL
    COUNT_ESTAB=$(echo "$RAW_TCP" | grep -c '^ESTAB' || true)
    COUNT_TIMEWAIT=$(echo "$RAW_TCP" | grep -c '^TIME-WAIT' || true)
    COUNT_CLOSEWAIT=$(echo "$RAW_TCP" | grep -c '^CLOSE-WAIT' || true)
    COUNT_LISTEN=$(echo "$RAW_TCP" | grep -c '^LISTEN' || true)
    COUNT_SYNSENT=$(echo "$RAW_TCP" | grep -c '^SYN-SENT' || true)
    COUNT_SYNRECV=$(echo "$RAW_TCP" | grep -c '^SYN-RECV' || true)
    COUNT_FINWAIT1=$(echo "$RAW_TCP" | grep -c '^FIN-WAIT-1' || true)
    COUNT_FINWAIT2=$(echo "$RAW_TCP" | grep -c '^FIN-WAIT-2' || true)
    COUNT_LASTACK=$(echo "$RAW_TCP" | grep -c '^LAST-ACK' || true)
    COUNT_TOTAL=$(echo "$RAW_TCP" | grep -v '^$' | wc -l || true)

    printf "   %-18s : ${GREEN}%d${NC}\n" "ESTABLISHED" "$COUNT_ESTAB"
    printf "   %-18s : %d\n" "LISTEN" "$COUNT_LISTEN"
    printf "   %-18s : %d\n" "TIME-WAIT" "$COUNT_TIMEWAIT"
    printf "   %-18s : %d\n" "CLOSE-WAIT" "$COUNT_CLOSEWAIT"
    printf "   %-18s : %d\n" "SYN-SENT" "$COUNT_SYNSENT"
    printf "   %-18s : %d\n" "SYN-RECV" "$COUNT_SYNRECV"
    printf "   %-18s : %d\n" "FIN-WAIT-1" "$COUNT_FINWAIT1"
    printf "   %-18s : %d\n" "FIN-WAIT-2" "$COUNT_FINWAIT2"
    printf "   %-18s : %d\n" "LAST-ACK" "$COUNT_LASTACK"
    echo "   ------------------------------------"
    printf "   %-18s : ${BOLD}%d${NC}\n" "TỔNG SỐ SOCKET TCP" "$COUNT_TOTAL"
    echo ""

    # Cảnh báo chẩn đoán tự động
    local HAS_WARNING=0
    if [[ "$COUNT_CLOSEWAIT" -gt 15 ]]; then
        log_warn "Phát hiện số lượng kết nối CLOSE-WAIT cao (${COUNT_CLOSEWAIT})."
        echo -e "       ${YELLOW}➔ Nguyên nhân:${NC} Ứng dụng nhận được tín hiệu đóng từ client nhưng chưa gọi close() socket."
        echo -e "       ${YELLOW}➔ Nguy cơ:${NC} Rò rỉ file descriptors (socket leak) dẫn tới lỗi 'Too many open files'."
        HAS_WARNING=1
    fi

    if [[ "$COUNT_TIMEWAIT" -gt 500 ]]; then
        log_warn "Phát hiện số lượng TIME-WAIT cao (${COUNT_TIMEWAIT})."
        echo -e "       ${YELLOW}➔ Nguyên nhân:${NC} Tạo và đóng kết nối TCP liên tục (high churn rate, không dùng HTTP Keep-Alive/Connection Pool)."
        echo -e "       ${YELLOW}➔ Nguy cơ:${NC} Cạn kiệt dải ephemeral ports nội bộ."
        HAS_WARNING=1
    fi

    if [[ "$COUNT_SYNSENT" -gt 10 ]]; then
        log_warn "Phát hiện nhiều kết nối đang ở trạng thái SYN-SENT (${COUNT_SYNSENT})."
        echo -e "       ${YELLOW}➔ Nguyên nhân:${NC} Kết nối ra bên ngoài bị drop (Firewall, sai port, DNS chậm hoặc destination service down)."
        HAS_WARNING=1
    fi

    if [[ "$COUNT_SYNRECV" -gt 10 ]]; then
        log_warn "Phát hiện nhiều kết nối SYN-RECV (${COUNT_SYNRECV})."
        echo -e "       ${YELLOW}➔ Nguyên nhân:${NC} Hàng đợi SYN bị nghẽn (SYN flood hoặc ứng dụng xử lý handshake không kịp)."
        HAS_WARNING=1
    fi
    [[ "$HAS_WARNING" -eq 1 ]] && echo ""

    # 3. Phân tích ứ đọng Hàng đợi (Queue Backlog / Congestion)
    echo -e "${BOLD}3. ⏳ HÀNG ĐỢI SOCKET Ứ ĐỌNG (Recv-Q & Send-Q Congestion):${NC}"
    log_cmd "   " "ss -tanp"
    local QUEUE_SOCKETS
    QUEUE_SOCKETS=$(ss -tanp 2>/dev/null | awk '
        NR>1 {
            # LISTEN socket: Recv-Q > 0 nghĩa là accept queue bị đầy
            # Non-listen socket: Recv-Q > 0 (app chưa đọc kịp) hoặc Send-Q > 0 (chưa ack xong)
            if (($1 == "LISTEN" && $2 > 0) || ($1 != "LISTEN" && ($2 > 0 || $3 > 0))) {
                print $0
            }
        }' || true)

    if [[ -z "$QUEUE_SOCKETS" ]]; then
        echo -e "   ${GREEN}✔ Tuyệt vời! Không có socket nào bị nghẽn hàng đợi (Recv-Q/Send-Q đều thông thoáng).${NC}"
    else
        log_warn "Phát hiện các kết nối đang bị ứ đọng trong hàng đợi:"
        echo -e "   ${YELLOW}(LISTEN với Recv-Q > 0 = App chưa kịp accept() | ESTAB với Send-Q > 0 = Nghẽn băng thông/bên nhận chậm)${NC}"
        printf "   %-10s %-8s %-8s %-25s %-25s %s\n" "STATE" "Recv-Q" "Send-Q" "LOCAL ADDRESS" "PEER ADDRESS" "PROCESS"
        echo "   ------------------------------------------------------------------------------------------------"
        echo "$QUEUE_SOCKETS" | while read -r line; do
            echo "   $line"
        done
    fi
    echo ""

    # 4. Các cổng đang mở Lắng nghe (Listening Ports)
    echo -e "${BOLD}4. 🎧 CÁC CỔNG ĐANG LẮNG NGHE (Listening Ports):${NC}"
    log_cmd "   " "ss -tulnp"
    local LISTEN_SOCKETS
    LISTEN_SOCKETS=$(ss -tulnp 2>/dev/null || ss -tuln 2>/dev/null || true)
    if [[ -n "$LISTEN_SOCKETS" ]]; then
        echo "$LISTEN_SOCKETS" | sed 's/^/   /'
    else
        echo "   (Không có cổng nào đang mở)"
    fi
    echo ""

    # 5. Top IP từ xa kết nối nhiều nhất
    echo -e "${BOLD}5. 🌐 TOP ${TOP_N} ĐỊA CHỈ IP TỪ XA KẾT NỐI NHIỀU NHẤT (Established):${NC}"
    log_cmd "   " "ss -tan state established"
    local TOP_IPS
    TOP_IPS=$(ss -tan state established 2>/dev/null | awk 'NR>1 {print $4}' | sed -E 's/:[0-9]+$//; s/^\[//; s/\]$//' | grep -v '^\*$' | grep -v '^$' | sort | uniq -c | sort -nr | head -n "$TOP_N" || true)

    if [[ -n "$TOP_IPS" ]]; then
        printf "   %-10s %s\n" "SỐ KẾT NỐI" "REMOTE IP"
        echo "   ------------------------------------"
        echo "$TOP_IPS" | while read -r count ip; do
            printf "   %-10s %s\n" "$count" "$ip"
        done
    else
        echo "   (Hiện không có kết nối ESTABLISHED nào từ remote IP)"
    fi
    echo ""
}

# Main Execution
if [[ -n "$FILTER_STATE" || -n "$FILTER_PORT" || "$FILTER_QUEUES" -eq 1 ]]; then
    run_filtered
elif [[ "$WATCH_MODE" -eq 1 ]]; then
    while true; do
        clear
        generate_report
        echo -e "${CYAN}Đang ở chế độ theo dõi liên tục (cập nhật mỗi ${WATCH_INTERVAL}s). Nhấn Ctrl+C để thoát...${NC}"
        sleep "$WATCH_INTERVAL"
    done
else
    generate_report
fi
