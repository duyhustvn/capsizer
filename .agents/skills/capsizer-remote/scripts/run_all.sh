#!/usr/bin/env bash
# ==============================================================================
# Script tự động hóa toàn bộ quy trình kiểm thử tải qua máy phát tải từ xa (One-Click)
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

RUN_ID="run_$(date +%Y%m%d_%H%M%S)"
OUTPUT_DIR="${PROJECT_ROOT}/results/${RUN_ID}"
mkdir -p "$OUTPUT_DIR"
echo "$OUTPUT_DIR" > "${PROJECT_ROOT}/.last_run_dir"

echo "================================================================================"
echo " CAPSIZER: KHỞI ĐỘNG QUY TRÌNH KIỂM THỬ TẢI QUA MÁY PHÁT TẢI TỪ XA"
echo " Thư mục phiên chạy: $OUTPUT_DIR"
echo "================================================================================"

cleanup() {
    echo ""
    echo "[CLEANUP] Thu thập file log và dừng tiến trình probe..."
    "$SCRIPT_DIR/manage_probe.sh" stop "$OUTPUT_DIR" || true
}
trap cleanup EXIT

# 1. Khởi động probe trên ứng dụng mục tiêu
echo ""
echo "--- GIAI ĐOẠN 1: KHỞI ĐỘNG PROBE TRÊN ỨNG DỤNG MỤC TIÊU ---"
"$SCRIPT_DIR/manage_probe.sh" start

# 2. Phát tải từ máy từ xa
echo ""
echo "--- GIAI ĐOẠN 2: PHÁT TẢI OPEN-LOOP TỪ MÁY TỪ XA ---"
OUTPUT_DIR="$OUTPUT_DIR" "$SCRIPT_DIR/run_remote.sh" --run

# 3. Thu thập probe (khi script kết thúc hoặc gặp lỗi, trap cleanup sẽ xử lý)
echo ""
echo "--- GIAI ĐOẠN 3: DỪNG PROBE & THU THẬP LOG ---"
"$SCRIPT_DIR/manage_probe.sh" stop "$OUTPUT_DIR"
trap - EXIT

# 4. Tạo báo cáo phân tích và sizing
echo ""
echo "--- GIAI ĐOẠN 4: TỔNG HỢP VÀ ĐỊNH CHUẨN NĂNG LỰC (CAPACITY SIZING) ---"
echo "Đang xử lý dữ liệu với report.go..."
(
    cd "${PROJECT_ROOT}"
    go run report.go \
      --ramp "${OUTPUT_DIR}/loadtest-ramp.jsonl" \
      --probe "${OUTPUT_DIR}/probe.jsonl"
)

echo ""
echo "================================================================================"
echo "[HOÀN TẤT] Dữ liệu chi tiết đã được lưu tại: ${OUTPUT_DIR}"
echo "================================================================================"
