#!/bin/sh
set -e

# ==============================================================================
# Script biên dịch ramps cho Docker Base và Derived images
# ==============================================================================
# 1. Tự động quét tất cả các folder dịch vụ trong scenarios/ (auth/, order/,...)
# 2. Sinh file loader.go để import các package tương ứng
# 3. Biên dịch binary tĩnh /usr/local/bin/capsizer-ramp
# 4. Lưu lại sha256 hash của mã nguồn
# ==============================================================================

APP_DIR="${1:-/app}"
if [ ! -d "$APP_DIR" ] || [ ! -f "$APP_DIR/main.go" ]; then
    if [ -f "ramps/golang/main.go" ]; then
        APP_DIR="ramps/golang"
    elif [ -f "main.go" ]; then
        APP_DIR="."
    fi
fi

BINARY_PATH="/usr/local/bin/capsizer-ramp"
HASH_FILE="/usr/local/bin/capsizer-ramp.sha256"

if [ -x "/usr/local/bin/update_loader" ]; then
    /usr/local/bin/update_loader "$APP_DIR"
elif [ -f "$(dirname "$0")/update_loader.sh" ]; then
    "$(dirname "$0")/update_loader.sh" "$APP_DIR"
fi

echo "==> [Capsizer] Đang biên dịch bộ phát tải..." >&2
(cd "$APP_DIR" && CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BINARY_PATH" .)

mkdir -p "$(dirname "$HASH_FILE")"
find "$APP_DIR" -name "*.go" -type f | sort | xargs cat 2>/dev/null | sha256sum | awk '{print $1}' > "$HASH_FILE"
echo "==> [Capsizer] Biên dịch thành công!" >&2
