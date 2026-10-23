#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build minimal sample-app fixture for upload/e2e tests.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="$SCRIPT_DIR/../examples/sample-app"

mkdir -p "$APP_DIR/data"

if [[ ! -f "$APP_DIR/sample-app.exe" ]]; then
  # Not a real PE — sufficient for scan/hash/upload tests.
  printf 'sample app stub for gdn e2e tests\n' >"$APP_DIR/sample-app.exe"
fi

if [[ ! -f "$APP_DIR/data/asset.bin" ]]; then
  dd if=/dev/urandom of="$APP_DIR/data/asset.bin" bs=256 count=4 status=none 2>/dev/null \
    || printf 'sample asset data\n' >"$APP_DIR/data/asset.bin"
fi

if [[ ! -f "$APP_DIR/data/readme.txt" ]]; then
  printf 'sample data readme\n' >"$APP_DIR/data/readme.txt"
fi

echo "sample-app ready: $APP_DIR"
