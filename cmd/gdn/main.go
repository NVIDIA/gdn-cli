// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/nvidia/gdn-cli/src/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
