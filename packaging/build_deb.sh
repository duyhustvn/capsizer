#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# Script đóng gói Capsizer thành file cài đặt Debian/Ubuntu (.deb)
# ==============================================================================
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

VERSION="${1:-1.0.0}"
ARCH="${2:-amd64}"
PKG_NAME="capsizer_${VERSION}_${ARCH}"
BUILD_ROOT="${PROJECT_ROOT}/build/deb/${PKG_NAME}"

echo "================================================================================"
echo "          ĐÓNG GÓI CAPSIZER THÀNH GÓI CÀI ĐẶT .DEB (VERSION: ${VERSION})"
echo "================================================================================"

rm -rf "${BUILD_ROOT}"
mkdir -p "${BUILD_ROOT}/DEBIAN"
mkdir -p "${BUILD_ROOT}/usr/bin"
mkdir -p "${BUILD_ROOT}/etc/capsizer"

echo "==> 1. Biên dịch tĩnh toàn bộ 3 binary (Go CGO_ENABLED=0)..."
(
  cd "$PROJECT_ROOT"
  echo "    [1/3] Biên dịch capsizer-ramp (Open-Loop Load Engine)..."
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -ldflags="-s -w" -o "${BUILD_ROOT}/usr/bin/capsizer-ramp" ./cmd/ramp

  echo "    [2/3] Biên dịch capsizer-probe (Kernel/cgroup System Daemon)..."
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -ldflags="-s -w" -o "${BUILD_ROOT}/usr/bin/capsizer-probe" ./cmd/probe

  echo "    [3/3] Biên dịch capsizer-report (Capacity Sizing Analyzer)..."
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -ldflags="-s -w" -o "${BUILD_ROOT}/usr/bin/capsizer-report" ./cmd/report
)

# Tạo alias tiện ích trong /usr/bin/
(
  cd "${BUILD_ROOT}/usr/bin"
  ln -sf capsizer-ramp capsizer
  ln -sf capsizer-probe probe
  ln -sf capsizer-report report
)

echo "==> 2. Tạo file DEBIAN/control..."
cat << EOF > "${BUILD_ROOT}/DEBIAN/control"
Package: capsizer
Version: ${VERSION}
Section: utils
Priority: optional
Architecture: ${ARCH}
Maintainer: Capsizer Maintainers <support@capsizer.io>
Description: High-performance Open-Loop capacity sizing, load testing and deep profiling toolchain.
 Capsizer provides:
  - capsizer-ramp: Goroutines-based open-loop load generator (declarative scenarios, SSE streaming, REST).
  - capsizer-probe: Ultralight system probe reading /proc and cgroups for throttling and accept queues.
  - capsizer-report: Automated capacity sizing calculator and saturation knee point detector.
EOF

echo "==> 3. Đóng gói file .deb bằng dpkg-deb..."
OUTPUT_DEB="${PROJECT_ROOT}/dist/${PKG_NAME}.deb"
mkdir -p "${PROJECT_ROOT}/dist"
dpkg-deb --root-owner-group --build "${BUILD_ROOT}" "${OUTPUT_DEB}"

echo ""
echo "================================================================================"
echo "✓ ĐÓNG GÓI HOÀN TẤT: ${OUTPUT_DEB}"
echo "Dung lượng gói: $(du -h "${OUTPUT_DEB}" | awk '{print $1}')"
echo "================================================================================"
echo ""
echo "HƯỚNG DẪN CÀI ĐẶT TRÊN MÁY BẤN TẢI / SERVER UBUNTU/DEBIAN:"
echo "  1. Copy file .deb sang máy khác:"
echo "     scp ${OUTPUT_DEB} user@remote-host:/tmp/"
echo ""
echo "  2. Cài đặt trực tiếp:"
echo "     sudo dpkg -i /tmp/${PKG_NAME}.deb"
echo ""
echo "  3. Chạy kịch bản khai báo declarative (như k6, không cần Go SDK):"
echo "     capsizer-ramp -scenario ./my-scenario.json -steps 2,4,8,16"
echo "================================================================================"
