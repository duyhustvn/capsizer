#!/usr/bin/env bash
set -euo pipefail

# Wrapper tương thích ngược: Chuyển tiếp toàn bộ thực thi Batch sang Golang Core CLI (./bin/capsizer)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

if [[ ! -x "${PROJECT_ROOT}/bin/capsizer" ]]; then
  (cd "$PROJECT_ROOT" && go build -o bin/capsizer ./cmd/capsizer)
fi

exec "${PROJECT_ROOT}/bin/capsizer" batch "$@"
