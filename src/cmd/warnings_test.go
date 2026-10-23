// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"
)

func TestFormatWarning(t *testing.T) {
	tests := []struct {
		name string
		w    cliWarning
		want string
	}{
		{
			name: "message only",
			w:    cliWarning{Message: "could not load env file"},
			want: "warning: could not load env file",
		},
		{
			name: "with suggestion",
			w: cliWarning{
				Message:    `"upload" is deprecated`,
				Suggestion: `use "gdn resource upload" instead`,
			},
			want: `warning: "upload" is deprecated; use "gdn resource upload" instead`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatWarning(tt.w); got != tt.want {
				t.Fatalf("formatWarning() = %q, want %q", got, tt.want)
			}
		})
	}
}
