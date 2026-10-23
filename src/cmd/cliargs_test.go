// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"reflect"
	"testing"
)

func TestNormalizeDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{`C:\Program Files\My App\`, `C:\Program Files\My App`},
		{`C:\Program Files\My App"`, `C:\Program Files\My App`},
		{`C:\Program Files\My App\"`, `C:\Program Files\My App`},
		{`"C:\Program Files\My App"`, `C:\Program Files\My App`},
		{`C:\`, `C:\`},
		{`/tmp/my-app/`, `/tmp/my-app`},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeDir(c.in); got != c.want {
			t.Errorf("normalizeDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitDirValueWithFlags(t *testing.T) {
	path, rest, ok := splitDirValueWithFlags(`C:\Program Files\My App\ --version 1.0.0 --name my-app-1`)
	if !ok {
		t.Fatal("expected split")
	}
	if path != `C:\Program Files\My App` {
		t.Errorf("path = %q", path)
	}
	want := []string{"--version", "1.0.0", "--name", "my-app-1"}
	if !reflect.DeepEqual(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}
}

func TestRepairDirArgs(t *testing.T) {
	in := []string{
		"--dir", `C:\Program Files\My App\ --version 1.0.0 --name my-app-1`,
	}
	want := []string{
		"--dir", `C:\Program Files\My App`,
		"--version", "1.0.0", "--name", "my-app-1",
	}
	if got := repairDirArgs(in); !reflect.DeepEqual(got, want) {
		t.Errorf("repairDirArgs() = %v, want %v", got, want)
	}
}

func TestSplitDirValueWithFlagsTrailingQuote(t *testing.T) {
	path, rest, ok := splitDirValueWithFlags(`C:\Program Files\My App" --version 1.0.0 --name my-app-1`)
	if !ok {
		t.Fatal("expected split")
	}
	if path != `C:\Program Files\My App` {
		t.Errorf("path = %q", path)
	}
	if len(rest) != 4 {
		t.Errorf("rest = %v", rest)
	}
}

func TestRepairDirArgsEqualsForm(t *testing.T) {
	in := []string{"--dir=C:\\Apps\\ --version 1.0.0"}
	want := []string{"--dir", `C:\Apps`, "--version", "1.0.0"}
	if got := repairDirArgs(in); !reflect.DeepEqual(got, want) {
		t.Errorf("repairDirArgs() = %v, want %v", got, want)
	}
}
