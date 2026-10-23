// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const offeredModes = `{"allowedDisplayModes":[
	{"MAX_RESOLUTION":"1080p","MAX_FPS":60},{"MAX_RESOLUTION":"1080p","MAX_FPS":120},
	{"MAX_RESOLUTION":"1440p","MAX_FPS":90},{"MAX_RESOLUTION":"4k","MAX_FPS":60}]}`

func TestParseDisplayMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want displayMode
	}{
		{"1080p@60", displayMode{MaxResolution: "1080p", MaxFPS: 60}},
		{" 4K @ 120 ", displayMode{MaxResolution: "4k", MaxFPS: 120}},
	} {
		got, err := parseDisplayMode(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseDisplayMode(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"1080p", "1080p@", "@60", "1080p@sixty", "1080p@0", "1080p@-5", "1080p:60"} {
		if _, err := parseDisplayMode(bad); err == nil || !strings.Contains(err.Error(), "expected RES@FPS") {
			t.Errorf("parseDisplayMode(%q) err = %v, want a format error", bad, err)
		}
	}
}

func TestDisplayModeListSet(t *testing.T) {
	var l displayModeList
	for _, v := range []string{"4k@60,1080p@120", "4K@60", "1440p@90"} {
		if err := l.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	if got, want := l.String(), "4k@60, 1080p@120, 1440p@90"; got != want {
		t.Errorf("list = %q, want %q (normalised, repeated mode kept once)", got, want)
	}
}

func TestFormatOfferedModes(t *testing.T) {
	var cfg appCreateConfig
	if err := json.Unmarshal([]byte(offeredModes), &cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := formatOfferedModes(cfg.AllowedDisplayModes), "1080p@60/120, 1440p@90, 4k@60"; got != want {
		t.Errorf("formatOfferedModes = %q, want %q", got, want)
	}
}

func TestCheckDisplayModes(t *testing.T) {
	serve := func(t *testing.T, status int, body string) *portalClient {
		srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return newPortal(srv.URL, "k", "o", secureClient)
	}
	ctx := context.Background()
	modes := []displayMode{{MaxResolution: "4k", MaxFPS: 60}, {MaxResolution: "5k", MaxFPS: 60}}

	err := serve(t, 200, offeredModes).checkDisplayModes(ctx, modes)
	if err == nil || !strings.Contains(err.Error(), "display mode(s) 5k@60 not offered by GDN; it offers 1080p@60/120, 1440p@90, 4k@60") {
		t.Errorf("unknown mode: err = %v", err)
	}
	if err := serve(t, 200, offeredModes).checkDisplayModes(ctx, modes[:1]); err != nil {
		t.Errorf("offered mode: err = %v, want nil", err)
	}
	if err := serve(t, 200, `{"allowedDisplayModes":[]}`).checkDisplayModes(ctx, modes); err != nil {
		t.Errorf("empty list: err = %v, want nil (sent unchecked)", err)
	}
	if err := serve(t, 403, `{}`).checkDisplayModes(ctx, modes); err != nil {
		t.Errorf("unreadable config: err = %v, want nil (sent unchecked)", err)
	}
}

func parseUpdateFlags(t *testing.T, args ...string) (*updateAppFlags, map[string]bool) {
	t.Helper()
	fs := flag.NewFlagSet("app update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	u := registerUpdateAppFlags(fs)
	if err := fs.Parse(append([]string{"--app-id", "a"}, args...)); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return u, set
}

func TestUpdateDisplayModeFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    []displayMode
		wantErr string
	}{
		{name: "replace", args: []string{"--display-mode", "4k@60", "--display-mode", "1080p@120"},
			want: []displayMode{{"4k", 60}, {"1080p", 120}}},
		{name: "deprecated pair", args: []string{"--resolution", "4k", "--max-fps", "60"},
			want: []displayMode{{"4k", 60}}},
		{name: "resolution alone", args: []string{"--resolution", "4k"}, wantErr: "must be given together"},
		{name: "max-fps alone", args: []string{"--max-fps", "120"}, wantErr: "must be given together"},
		{name: "negative fps", args: []string{"--resolution", "4k", "--max-fps=-5"}, wantErr: "expected RES@FPS"},
		{name: "pair with display-mode", args: []string{"--display-mode", "4k@60", "--resolution", "4k", "--max-fps", "60"}, wantErr: "cannot be combined with --display-mode"},
		{name: "also in app-features", args: []string{"--display-mode", "4k@60", "--app-features", `{"DISPLAY_MODE":[{"MAX_RESOLUTION":"1080p","MAX_FPS":60}]}`}, wantErr: "both set the display modes"},
		{name: "partial app-features entry", args: []string{"--app-features", `{"DISPLAY_MODE":[{"MAX_FPS":120}]}`}, wantErr: "each DISPLAY_MODE entry needs MAX_RESOLUTION and MAX_FPS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, set := parseUpdateFlags(t, tc.args...)
			feat, err := u.features(set)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("features: %v", err)
			}
			if feat == nil || !slices.Equal(feat.DisplayMode, tc.want) {
				t.Errorf("DISPLAY_MODE = %+v, want %+v", feat, tc.want)
			}
		})
	}
}

func TestRunUpdateAppDisplayModeDryRun(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = runUpdateApp([]string{"--app-id", "app-1", "--display-mode", "4k@60,1080p@120", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("runUpdateApp: %v", err)
	}
	if !strings.Contains(out, "display modes: 4k@60, 1080p@120 (replaces the current list)") {
		t.Errorf("dry-run output lacks the full list:\n%s", out)
	}
}

func TestRunUpdateAppChecksDisplayModes(t *testing.T) {
	patched := false
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/gdn/apps/create-config":
			writeJSON(w, offeredModes)
		case r.Method == http.MethodPatch:
			patched = true
			writeJSON(w, `{"id":"app-1"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_BASE_URL", srv.URL)

	err := runUpdateApp([]string{"--app-id", "app-1", "--display-mode", "5k@60"})
	if err == nil || !strings.Contains(err.Error(), "5k@60 not offered by GDN") {
		t.Errorf("err = %v, want the not-offered error", err)
	}
	if patched {
		t.Error("app update must not PATCH a display mode GDN does not offer")
	}
}

func TestRunCreateAppSendsDisplayModes(t *testing.T) {
	var createRaw []byte
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/gdn/apps/create-config":
			writeJSON(w, `{"allowedInstanceNames":["NVIDIA L40"],`+strings.TrimPrefix(offeredModes, "{"))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps":
			writeJSON(w, `{"applications":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orgs/o/gdn/apps":
			createRaw, _ = io.ReadAll(r.Body)
			writeJSON(w, `{"id":"app-9","versions":[{"id":"ver-1"}]}`)
		case r.Method == http.MethodPatch:
			writeJSON(w, `{"id":"app-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_BASE_URL", srv.URL)

	var err error
	captureOutput(t, func() {
		err = runCreateApp([]string{"--app-name", "my-app", "--version", "1.0.0",
			"--resource", "my-app:1.0.0", "--exec-path", "app.exe",
			"--display-mode", "4k@60", "--display-mode", "1080p@120"})
	})
	if err != nil {
		t.Fatalf("runCreateApp: %v", err)
	}
	var body createAppReq
	if err := json.Unmarshal(createRaw, &body); err != nil {
		t.Fatalf("create body: %v (%s)", err, createRaw)
	}
	want := []displayMode{{"4k", 60}, {"1080p", 120}}
	if body.ApplicationFeatures == nil || !slices.Equal(body.ApplicationFeatures.DisplayMode, want) {
		t.Errorf("DISPLAY_MODE = %+v, want %+v", body.ApplicationFeatures, want)
	}
}

func TestEditDisplayModes(t *testing.T) {
	current := []displayMode{{"4k", 60}, {"1080p", 120}}
	for _, tc := range []struct {
		name        string
		add, remove []displayMode
		want        []displayMode
		wantNotes   int
		wantErr     string
	}{
		{name: "add", add: []displayMode{{"1440p", 90}}, want: []displayMode{{"4k", 60}, {"1080p", 120}, {"1440p", 90}}},
		{name: "remove", remove: []displayMode{{"4k", 60}}, want: []displayMode{{"1080p", 120}}},
		{name: "swap", remove: []displayMode{{"4k", 60}}, add: []displayMode{{"4k", 30}}, want: []displayMode{{"1080p", 120}, {"4k", 30}}},
		{name: "add existing", add: []displayMode{{"4k", 60}}, want: current, wantNotes: 1},
		{name: "remove missing", remove: []displayMode{{"720p", 30}}, want: current, wantNotes: 1},
		{name: "remove all", remove: current, wantErr: "would leave no display modes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, notes, err := editDisplayModes(current, tc.add, tc.remove)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) || len(notes) != tc.wantNotes {
				t.Errorf("got %v, notes %v, err %v; want %v with %d note(s)", got, notes, err, tc.want, tc.wantNotes)
			}
		})
	}
	if len(current) != 2 {
		t.Errorf("editDisplayModes changed its input: %v", current)
	}

	// An app with no display modes: removing changes nothing, adding works.
	none := []displayMode{}
	if got, notes, err := editDisplayModes(none, nil, []displayMode{{"4k", 60}}); err != nil || len(got) != 0 || len(notes) != 1 {
		t.Errorf("remove from empty: got %v, notes %v, err %v; want no change and one note", got, notes, err)
	}
	if got, _, err := editDisplayModes(none, []displayMode{{"4k", 60}}, nil); err != nil || !slices.Equal(got, []displayMode{{"4k", 60}}) {
		t.Errorf("add to empty: got %v, err %v; want [4k@60]", got, err)
	}
}

func TestGetCreateConfigReadsOnce(t *testing.T) {
	hits := 0
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeJSON(w, offeredModes)
	}))
	defer srv.Close()
	p := newPortal(srv.URL, "k", "o", srv.Client())
	for range 2 {
		if _, err := p.getCreateConfig(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if hits != 1 {
		t.Errorf("create-config read %d times, want 1", hits)
	}
}

// editServer serves an app whose display modes are 4k@60 and 1080p@120, and
// records the PATCH body.
func editServer(t *testing.T, patched *[]byte) {
	t.Helper()
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/gdn/apps/create-config":
			writeJSON(w, offeredModes)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps/app-1":
			writeJSON(w, `{"id":"app-1","name":"my-app","applicationFeatures":{"DISPLAY_MODE":[{"MAX_RESOLUTION":"4k","MAX_FPS":60},{"MAX_RESOLUTION":"1080p","MAX_FPS":120}]}}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/v2/orgs/o/gdn/apps/app-1":
			*patched, _ = io.ReadAll(r.Body)
			writeJSON(w, `{"id":"app-1","name":"my-app"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_BASE_URL", srv.URL)
}

func TestRunUpdateAppAddDisplayMode(t *testing.T) {
	var patched []byte
	editServer(t, &patched)
	var err error
	out := captureStdout(t, func() {
		err = runUpdateApp([]string{"--app-id", "app-1", "--add-display-mode", "1440p@90"})
	})
	if err != nil {
		t.Fatalf("runUpdateApp: %v", err)
	}
	want := `"DISPLAY_MODE":[{"MAX_RESOLUTION":"4k","MAX_FPS":60},{"MAX_RESOLUTION":"1080p","MAX_FPS":120},{"MAX_RESOLUTION":"1440p","MAX_FPS":90}]`
	if !strings.Contains(string(patched), want) {
		t.Errorf("PATCH body = %s, want the current list plus 1440p@90", patched)
	}
	if !strings.Contains(out, "display modes: 4k@60, 1080p@120, 1440p@90") {
		t.Errorf("output lacks the new list:\n%s", out)
	}
}

func TestRunUpdateAppRemoveDisplayModeDryRun(t *testing.T) {
	var patched []byte
	editServer(t, &patched)
	var err error
	out := captureStdout(t, func() {
		err = runUpdateApp([]string{"--app-id", "app-1", "--remove-display-mode", "4k@60", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("runUpdateApp: %v", err)
	}
	if patched != nil {
		t.Errorf("dry run sent a PATCH: %s", patched)
	}
	if !strings.Contains(out, "before: 4k@60, 1080p@120\n    after:  1080p@120") {
		t.Errorf("dry-run output lacks before/after:\n%s", out)
	}
}

func TestRunUpdateAppDisplayModeEditNoOp(t *testing.T) {
	var patched []byte
	editServer(t, &patched)

	var err error
	out := captureStdout(t, func() {
		err = runUpdateApp([]string{"--app-id", "app-1", "--add-display-mode", "4k@60"})
	})
	if err != nil || patched != nil || !strings.Contains(out, "Nothing to update") {
		t.Errorf("err = %v, patched = %s, out = %q; want no PATCH and a nothing-to-update note", err, patched, out)
	}

	// With another field set, the update still goes out without DISPLAY_MODE.
	out = captureStdout(t, func() {
		err = runUpdateApp([]string{"--app-id", "app-1", "--app-name", "renamed", "--add-display-mode", "4k@60"})
	})
	if err != nil {
		t.Fatalf("runUpdateApp: %v", err)
	}
	if strings.Contains(string(patched), "DISPLAY_MODE") || !strings.Contains(string(patched), `"name":"renamed"`) {
		t.Errorf("PATCH body = %s, want the name only", patched)
	}
}

func TestRunUpdateAppEditRejectsReplace(t *testing.T) {
	t.Setenv("NGC_API_KEY", "")
	t.Setenv("NGC_ORG", "")
	var err error
	_, stderr := captureOutput(t, func() {
		err = runUpdateApp([]string{"--app-id", "app-1", "--display-mode", "4k@60", "--add-display-mode", "1440p@90", "--dry-run"})
	})
	if err == nil || !strings.Contains(stderr+err.Error(), "cannot be combined with --display-mode") {
		t.Errorf("err = %v, stderr = %q; want the set/edit conflict, reported before the missing credentials", err, stderr)
	}
}
