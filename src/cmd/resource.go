// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import "github.com/spf13/cobra"

func newResourceCommand() *cobra.Command {
	resource := newGroupCommand("resource", "Manage NGC artifact registry resources")
	resource.AddCommand(leafCommand("upload", "Upload a folder to NGC as an artifact version", runUpload))
	resource.AddCommand(leafCommand("update", "Create a new version from a base version plus added files", runUpdateResource))
	resource.AddCommand(leafCommand("download", "Download a resource version's files from NGC", runDownloadResource))
	resource.AddCommand(leafCommand("versions", "List the versions of an NGC resource", runListResourceVersions))
	resource.AddCommand(leafCommand("delete", "Delete an NGC resource version or whole resource", runDeleteResource))
	resource.AddCommand(leafCommand("list", "List NGC resources in the org", runListResources))
	return resource
}
