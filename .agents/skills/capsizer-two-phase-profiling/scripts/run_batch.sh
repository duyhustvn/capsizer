#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# Capsizer Batch Runner: Tự động chạy tuần tự nhiều kịch bản (Multi-Scenario)
# ==============================================================================
# Cách dùng:
#   1. Chạy tất cả kịch bản có trong thư mục scenarios/:
#      ./run_batch.sh
#   2. Chạy danh sách kịch bản cụ thể:
#      ./run_batch.sh chatbot sample-post
# ==============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

RUN_RUNNER="${SCRIPT_DIR}/run_two_phase.sh"
SCENARIOS_DIR="${PROJECT_ROOT}/scenarios"

TARGET_SCENARIOS=("$@")

if [[ ${#TARGET_SCENARIOS[@]} -eq 0 ]]; then
  echo "==> Không chỉ định kịch bản cụ thể. Đang quét thư mục: $SCENARIOS_DIR"
  for s_dir in "$SCENARIOS_DIR"/*; do
    if [[ -d "$s_dir" && (-f "$s_dir/scenario.env" || -f "$s_dir/.env") ]]; then
      TARGET_SCENARIOS+=("$(basename "$s_dir")")
    fi
  done
fi

if [[ ${#TARGET_SCENARIOS[@]} -eq 0 ]]; then
  echo "[ERROR] Không tìm thấy kịch bản nào trong $SCENARIOS_DIR!" >&2
  exit 1
fi

BATCH_ID="batch_$(date +%Y%m%d_%H%M%S)"
BATCH_LOG_DIR="${PROJECT_ROOT}/benchmark-results/${BATCH_ID}"
mkdir -p "$BATCH_LOG_DIR"

echo "================================================================================"
echo "    CAPSIZER BATCH RUNNER: CHẠY TỰ ĐỘNG ${#TARGET_SCENARIOS[@]} KỊCH BẢN"
echo "================================================================================"
echo "Danh sách kịch bản : ${TARGET_SCENARIOS[*]}"
echo "Thư mục Batch Log  : $BATCH_LOG_DIR"
echo "================================================================================"

SUMMARY_FILE="$BATCH_LOG_DIR/batch_summary.txt"
{
  echo "================================================================================"
  echo "          BẢNG TỔNG HỢP SO SÁNH NĂNG LỰC CÁC DỊCH VỤ (BATCH RUN)"
  echo "================================================================================"
  printf "%-18s | %-12s | %-15s | %-12s | %-12s | %-10s\n" \
    "SCENARIO" "PROFILE" "KNEE POINT" "P95 LATENCY" "STATUS" "REPORT LINK"
  echo "--------------------------------------------------------------------------------"
} > "$SUMMARY_FILE"

INDEX=1
TOTAL=${#TARGET_SCENARIOS[@]}

for sc in "${TARGET_SCENARIOS[@]}"; do
  echo ""
  echo ">>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>"
  echo "[$INDEX/$TOTAL] BẮT ĐẦU CHẠY KỊCH BẢN: $sc"
  echo ">>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>>"

  SC_OUT="$BATCH_LOG_DIR/$sc"
  mkdir -p "$SC_OUT"

  set +e
  "$RUN_RUNNER" --scenario "$sc" --out-dir "$SC_OUT" | tee "$SC_OUT/console.log"
  EXIT_CODE=$?
  set -e

  # Trích xuất số liệu từ phase1-report.txt nếu có
  REPORT_FILE="$SC_OUT/phase1-report.txt"
  KNEE_STR="None"
  P95_STR="-"
  STATUS_STR="FAILED"

  if [[ $EXIT_CODE -eq 0 ]]; then
    STATUS_STR="SUCCESS"
  fi

  if [[ -f "$REPORT_FILE" ]]; then
    KNEE_RPS=$(grep -E "Điểm gãy quan sát được" "$REPORT_FILE" 2>/dev/null | grep -oE "[0-9]+(\.[0-9]+)? req/s" | head -n 1 || true)
    if [[ -n "$KNEE_RPS" ]]; then
      KNEE_STR="$KNEE_RPS"
    else
      KNEE_STR="Khỏe (No Knee)"
    fi
  fi

  printf "%-18s | %-12s | %-15s | %-12s | %-12s | %-10s\n" \
    "$sc" "-" "$KNEE_STR" "$P95_STR" "$STATUS_STR" "$SC_OUT/phase1-report.txt" >> "$SUMMARY_FILE"

  INDEX=$((INDEX + 1))
done

echo ""
cat "$SUMMARY_FILE"
echo ""
echo "================================================================================"
echo "✓ HOÀN THÀNH TOÀN BỘ PHIÊN CHẠY BATCH!"
echo "Báo cáo tổng hợp: $SUMMARY_FILE"
echo "================================================================================"
