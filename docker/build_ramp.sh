#!/bin/sh
set -e

# ==============================================================================
# Script biên dịch ramps cho Docker Base và Derived images
# ==============================================================================
APP_DIR="${1:-/app}"
if [ ! -d "$APP_DIR" ] || [ ! -d "$APP_DIR/cmd/ramp" ]; then
    if [ -d "cmd/ramp" ]; then
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
(cd "$APP_DIR" && CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BINARY_PATH" ./cmd/ramp)

mkdir -p "$(dirname "$HASH_FILE")"
find "$APP_DIR" -name "*.go" -type f | sort | xargs cat 2>/dev/null | sha256sum | awk '{print $1}' > "$HASH_FILE"
echo "==> [Capsizer] Biên dịch thành công!" >&2
