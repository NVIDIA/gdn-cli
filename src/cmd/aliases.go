// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import "github.com/spf13/cobra"

type legacyCommand struct {
	use         string
	replacement string
	run         func([]string) error
}

var legacyCommands = []legacyCommand{
	{use: "create-app", replacement: "app create", run: runCreateApp},
	{use: "update-app", replacement: "app update", run: runUpdateApp},
	{use: "clone-app", replacement: "app clone", run: runCloneApp},
	{use: "delete-app", replacement: "app delete", run: runDeleteApp},
	{use: "launch-app", replacement: "app launch", run: runLaunchApp},
	{use: "add-version", replacement: "app version add", run: runAddVersion},
	{use: "upload", replacement: "resource upload", run: runUpload},
	{use: "delete-resource", replacement: "resource delete", run: runDeleteResource},
	{use: "list-resources", replacement: "resource list", run: runListResources},
}

func addLegacyCommands(root *cobra.Command) {
	for _, legacy := range legacyCommands {
		legacy := legacy
		cmd := leafCommand(legacy.use, "Deprecated alias for "+legacy.replacement, func(args []string) error {
			warnDeprecatedCommand(legacy.use, legacy.replacement)
			return legacy.run(args)
		})
		cmd.Hidden = true
		root.AddCommand(cmd)
	}
}
