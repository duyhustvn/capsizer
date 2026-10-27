#!/bin/sh
# Dynamic wrapper for perf to bypass Ubuntu kernel version check in containers

# 1. Check if exact matching kernel perf binary exists in common directories
if [ -x "/usr/lib/linux-tools/$(uname -r)/perf" ]; then
    exec "/usr/lib/linux-tools/$(uname -r)/perf" "$@"
fi

KERNEL_BASE=$(echo "$(uname -r)" | sed 's/-generic//;s/-aws//;s/-gcp//;s/-oracle//;s/-azure//')
if [ -x "/usr/lib/linux-tools/${KERNEL_BASE}/perf" ]; then
    exec "/usr/lib/linux-tools/${KERNEL_BASE}/perf" "$@"
fi

# 2. Fallback to any installed perf binary under /usr/lib (including /usr/lib/linux-tools-*)
FALLBACK_PERF=$(find /usr/lib /usr/libexec -name perf 2>/dev/null | grep -v '/usr/bin/' | grep -v '/usr/local/bin/' | head -n 1)

if [ -n "$FALLBACK_PERF" ] && [ -x "$FALLBACK_PERF" ]; then
    exec "$FALLBACK_PERF" "$@"
fi

echo "Error: No perf executable found under /usr/lib in container." >&2
echo "Files found matching *perf* under /usr/lib:" >&2
find /usr/lib -name "*perf*" 2>/dev/null >&2
exit 1
