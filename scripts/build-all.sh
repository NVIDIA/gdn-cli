#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Cross-compile gdn into bin/. Usage: ./scripts/build-all.sh
set -euo pipefail

cd "$(dirname "$0")/.."

BIN=bin NAME=gdn
BUILD_TAGS="${BUILD_TAGS:-netgo osusergo}"
LD_FLAGS="-s -w -buildid="
command -v go >/dev/null || { echo "go not found" >&2; exit 1; }
mkdir -p "$BIN"

build() {
  echo "building $1/$2 -> $3"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -trimpath -buildvcs=false -tags "$BUILD_TAGS" -ldflags "$LD_FLAGS" -o "$3" ./cmd/gdn
}

build linux  amd64 "$BIN/${NAME}-linux-amd64"
build linux  arm64 "$BIN/${NAME}-linux-arm64"
build darwin amd64 "$BIN/${NAME}-darwin-amd64"
build darwin arm64 "$BIN/${NAME}-darwin-arm64"
build windows amd64 "$BIN/${NAME}-windows-amd64.exe"

echo "done — binaries in $BIN/"; ls -lh "$BIN"/${NAME}-*
