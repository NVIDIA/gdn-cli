// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

// Flag names registered by more than one command. Most are also looked up by
// name in the fs.Visit set map after parsing, so sharing one constant is what
// keeps the registration and the lookup from drifting apart.
const (
	flagAppID             = "app-id"
	flagAppName           = "app-name"
	flagAppFeatures       = "app-features"
	flagSeatType          = "seat-type"
	flagSetLatest         = "set-latest"
	flagDryRun            = "dry-run"
	flagUpstreamMediaType = "upstream-media-type"
	flagHashWorkers       = "hash-workers"
	flagMultipartWorkers  = "multipart-workers"
	flagFileWorkers       = "file-workers"
	flagChunkWorkers      = "chunk-workers"
	flagSkipExisting      = "skip-existing"
	flagHelpAll           = "help-all"
	flagDisplayMode       = "display-mode"
	flagResolution        = "resolution"
	flagMaxFPS            = "max-fps"
)

// dryRunPlaceholder stands in for the org or API key on a dry run, which needs
// neither but still builds the same coordinates and client a real run does.
// It happens to read the same as flagDryRun; the two are unrelated.
const dryRunPlaceholder = "dry-run"

// Flag descriptions shared verbatim by more than one command.
const (
	usageOrg              = "NGC org name (env NGC_ORG)"
	usageHashWorkers      = "(optional) parallel file-hashing workers (0 = one per CPU)"
	usageMultipartWorkers = "(optional) parallel S3 part upload workers per file (0 = one per CPU)"
	usageFileWorkers      = "(optional) parallel file upload workers"
	usageDownloadWorkers  = "(optional) parallel file download workers"
	usageChunkWorkers     = "(optional) parallel range download workers per large file (0 = one per CPU)"
	usageSkipExisting     = "(optional) skip files whose local copy already matches the registry checksum"
	usageDryRunScan       = "scan and hash files, print the plan, make no API calls"
	usageJSONResult       = "output result as JSON instead of human-readable summary"
	usageIgnoredOnAdd     = "ignored when adding a version"
)

// Help lines shared by more than one command's usage block. The padding is
// part of the string: printHelpSections writes each line verbatim, so these
// have to stay column-aligned with the hand-written lines beside them.
const (
	helpLineOrg         = "--org ORG               NGC org name (env NGC_ORG)"
	helpLineJSONMachine = "--json                  output machine-readable JSON"
	helpLineDryRunScan  = "--dry-run               scan and print the plan, make no API calls"
	helpLineNoSetLatest = "--set-latest=false      do not mark the NGC version as latest"
)

// cmdResourceList is quoted back to the user in hints and error messages.
const cmdResourceList = "gdn resource list"
