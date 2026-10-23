#requires -Version 5.1
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build minimal sample-app fixture for upload/e2e tests.
$ErrorActionPreference = 'Stop'

$AppDir = Join-Path $PSScriptRoot '..\examples\sample-app'
New-Item -ItemType Directory -Force -Path (Join-Path $AppDir 'data') | Out-Null

$Exe = Join-Path $AppDir 'sample-app.exe'
if (-not (Test-Path -LiteralPath $Exe)) {
    # Not a real PE — sufficient for scan/hash/upload tests.
    Set-Content -LiteralPath $Exe -Value 'sample app stub for gdn e2e tests' -NoNewline
}

$Asset = Join-Path $AppDir 'data\asset.bin'
if (-not (Test-Path -LiteralPath $Asset)) {
    Set-Content -LiteralPath $Asset -Value 'sample asset data' -NoNewline
}

$Readme = Join-Path $AppDir 'data\readme.txt'
if (-not (Test-Path -LiteralPath $Readme)) {
    Set-Content -LiteralPath $Readme -Value 'sample data readme'
}

Write-Host "sample-app ready: $AppDir"
