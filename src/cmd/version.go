// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"flag"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.Usage = func() {
		printCommandHelp(os.Stderr, fs, "version", "Print the gdn build version.",
			nil,
			[]string{
				"gdn version",
				"gdn -v",
			},
			false,
		)
	}
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	if fs.NArg() > 0 {
		return failUsage(fs, "version does not accept arguments")
	}
	fmt.Println(version)
	return nil
}

func newVersionCommand() *cobra.Command {
	return leafCommand("version", "Print the gdn version", runVersion)
}
