#!/bin/sh
set -e

# ==============================================================================
# Capsizer Ramps Smart Entrypoint
# ==============================================================================
BINARY_PATH="/usr/local/bin/capsizer-ramp"
HASH_FILE="/usr/local/bin/capsizer-ramp.sha256"

if [ -d "/app" ] && [ -d "/app/cmd/ramp" ]; then
    CURRENT_HASH=$(find /app -name "*.go" -type f | sort | xargs cat 2>/dev/null | sha256sum | awk '{print $1}')
    SAVED_HASH=""
    if [ -f "$HASH_FILE" ]; then
        SAVED_HASH=$(cat "$HASH_FILE" 2>/dev/null || true)
    fi

    if [ ! -f "$BINARY_PATH" ] || [ "$CURRENT_HASH" != "$SAVED_HASH" ] || [ "$FORCE_REBUILD" = "1" ]; then
        echo "==> [Capsizer] Phát hiện scenario hoặc mã nguồn mới..." >&2
        if [ -x "/usr/local/bin/build_ramp" ]; then
            /usr/local/bin/build_ramp /app
        else
            (cd /app && CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BINARY_PATH" ./cmd/ramp)
            echo "$CURRENT_HASH" > "$HASH_FILE"
        fi
    fi
fi

exec "$BINARY_PATH" "$@"
