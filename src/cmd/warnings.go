// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"
)

type cliWarning struct {
	Message    string
	Suggestion string
}

func emitWarning(w cliWarning) {
	fmt.Fprintln(os.Stderr, formatWarning(w))
}

func formatWarning(w cliWarning) string {
	if w.Suggestion == "" {
		return "warning: " + w.Message
	}
	return fmt.Sprintf("warning: %s; %s", w.Message, w.Suggestion)
}

func warnDeprecatedCommand(alias, replacement string) {
	emitWarning(cliWarning{
		Message:    fmt.Sprintf("%q is deprecated", alias),
		Suggestion: fmt.Sprintf("use \"gdn %s\" instead", replacement),
	})
}

func warnf(format string, args ...any) {
	emitWarning(cliWarning{Message: fmt.Sprintf(format, args...)})
}
