// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestInitialVersionID(t *testing.T) {
	withVersions := gdnApp{DefaultVersionID: "default", Versions: []gdnVersion{{ID: "v1"}}}
	if got := withVersions.initialVersionID(); got != "v1" {
		t.Errorf("got %q, want v1", got)
	}
	fallback := gdnApp{DefaultVersionID: "default"}
	if got := fallback.initialVersionID(); got != "default" {
		t.Errorf("got %q, want default", got)
	}
}

func TestCreateApp(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody createAppReq
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		writeJSON(w, `{"id":"app-1","versions":[{"id":"ver-1"}]}`)
	}))
	defer srv.Close()

	c := newPortal(srv.URL, "secret", "myorg", secureClient)
	app, err := c.createApp(context.Background(), createAppReq{
		Name:          "My App",
		SeatType:      "WINDOWS",
		InstanceNames: []string{"NVIDIA T5"},
		Version: createVersionReq{
			LaunchConfig: launchConfig{ExecutablePath: `%GDN_ROOT%\RESOURCE\my-app-1\app.exe`},
			Artifacts:    []artifactRef{{Name: "my-app-1", OrgName: "myorg", Type: "RESOURCE", VersionID: "1.0.0"}},
		},
	})
	if err != nil {
		t.Fatalf("createApp: %v", err)
	}

	if gotPath != "/v2/orgs/myorg/gdn/apps" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody.Name != "My App" || len(gotBody.Version.Artifacts) != 1 || gotBody.Version.Artifacts[0].VersionID != "1.0.0" {
		t.Errorf("unexpected body: %+v", gotBody)
	}
	if app.ID != "app-1" || app.initialVersionID() != "ver-1" {
		t.Errorf("app = %+v", app)
	}
}

func TestFindAppByName(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"applications":[{"id":"app-1","name":"My App"},{"id":"app-2","name":"Other"}]}`)
	}))
	defer srv.Close()

	c := newPortal(srv.URL, "k", "o", secureClient)
	// Match is case-insensitive.
	got, err := c.findAppByName(context.Background(), "my app")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "app-1" {
		t.Errorf("got %+v, want app-1", got)
	}
	// No match returns nil, nil.
	got, err = c.findAppByName(context.Background(), "Nope")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestCreateVersionPath(t *testing.T) {
	var gotPath string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, `{"id":"ver-2","status":"CREATED"}`)
	}))
	defer srv.Close()

	c := newPortal(srv.URL, "k", "o", secureClient)
	ver, err := c.createVersion(context.Background(), "app-9", createVersionReq{})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v2/orgs/o/gdn/apps/app-9/versions" {
		t.Errorf("path = %q", gotPath)
	}
	if ver.ID != "ver-2" {
		t.Errorf("version id = %q", ver.ID)
	}
}

func TestDeleteApp(t *testing.T) {
	var gotMethod, gotPath string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newPortal(srv.URL, "k", "o", secureClient).deleteApp(context.Background(), "app-7"); err != nil {
		t.Fatalf("deleteApp: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v2/orgs/o/gdn/apps/app-7" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
}

func TestUpdateApp(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody updateAppReq
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		writeJSON(w, `{"id":"app-5","name":"Updated App"}`)
	}))
	defer srv.Close()

	c := newPortal(srv.URL, "secret", "myorg", secureClient)
	app, err := c.updateApp(context.Background(), "app-5", updateAppReq{
		Name:          "Updated App",
		InstanceNames: []string{"NVIDIA A10", "NVIDIA L20"},
	})
	if err != nil {
		t.Fatalf("updateApp: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/v2/orgs/myorg/gdn/apps/app-5" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody.Name != "Updated App" || len(gotBody.InstanceNames) != 2 || gotBody.InstanceNames[0] != "NVIDIA A10" {
		t.Errorf("body = %+v", gotBody)
	}
	if app.ID != "app-5" || app.Name != "Updated App" {
		t.Errorf("app = %+v", app)
	}
}

// TestUpdateAppFeatureOmitsNilFields verifies that unset pointer fields
// (bool and uint64) are absent from the JSON body so unintended zero values
// are never sent to the API during a partial update.
func TestUpdateAppFeatureOmitsNilFields(t *testing.T) {
	bitDepth := uint64(10)
	req := updateAppReq{
		ApplicationFeatures: &appFeature{
			MaxBitDepth: &bitDepth,
			QosPolicy:   "QUALITY",
			// TouchSupport, RuntimeDisplayModeChange left nil
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)

	// Nil pointer fields must be absent.
	for _, absent := range []string{"TOUCH_SUPPORT", "RUNTIME_DISPLAY_MODE_CHANGE", "MAX_BIT_RATE", "IDLE_TIMEOUT", "SESSION_TIME_LIMIT"} {
		if strings.Contains(s, absent) {
			t.Errorf("field %q should be omitted when nil, got: %s", absent, s)
		}
	}
	// Empty string fields must be absent.
	for _, absent := range []string{`"name"`, `"seatType"`, `"description"`, `"SUPPORTED_CHROMA_FORMAT"`, `"INSTANCE_REUSE_POLICY"`} {
		if strings.Contains(s, absent) {
			t.Errorf("field %q should be omitted when empty, got: %s", absent, s)
		}
	}
	// Set fields must be present.
	if !strings.Contains(s, `"MAX_BIT_DEPTH":10`) {
		t.Errorf("MAX_BIT_DEPTH missing from: %s", s)
	}
	if !strings.Contains(s, `"QOS_POLICY":"QUALITY"`) {
		t.Errorf("QOS_POLICY missing from: %s", s)
	}
}

// TestUpdateAppBoolFalseIsNotOmitted verifies that a *bool field explicitly
// set to false is included in the JSON (not omitted like an unset nil).
func TestUpdateAppBoolFalseIsNotOmitted(t *testing.T) {
	f := false
	req := updateAppReq{
		ApplicationFeatures: &appFeature{TouchSupport: &f},
	}
	raw, _ := json.Marshal(req)
	s := string(raw)
	if !strings.Contains(s, `"TOUCH_SUPPORT":false`) {
		t.Errorf("TOUCH_SUPPORT:false should be present when pointer is set, got: %s", s)
	}
}

func TestSetDefaultVersion(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := newPortal(srv.URL, "k", "myorg", secureClient).setDefaultVersion(context.Background(), "app-3", "ver-7")
	if err != nil {
		t.Fatalf("setDefaultVersion: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/v2/orgs/myorg/gdn/apps/app-3" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["defaultVersionId"] != "ver-7" {
		t.Errorf("body = %+v, want defaultVersionId=ver-7", gotBody)
	}
}

func TestGetAppParsesSettings(t *testing.T) {
	var gotPath string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// Mirrors the current GDN payload: the app embeds only the current
		// (default) version, in full, alongside the total versionCount.
		writeJSON(w, `{
			"id":"app-1","name":"My App","description":"d","seatType":"LINUX",
			"instanceNames":["NVIDIA A10","NVIDIA L20"],
			"applicationFeatures":{"DISPLAY_MODE":[{"MAX_RESOLUTION":"4K","MAX_FPS":120}],"RTX_ENABLED":true,"MAX_BIT_RATE":200},
			"defaultVersionId":"ver-2",
			"versionCount":2,
			"versions":[
				{"id":"ver-2","versionName":"1.1.0","launchConfig":{"executablePath":"%GDN_ROOT%\\RESOURCE\\my-app-1\\app.exe"},"artifacts":[{"name":"my-app-1","orgName":"o","type":"RESOURCE","versionId":"1.1.0"}]}
			]
		}`)
	}))
	defer srv.Close()

	app, err := newPortal(srv.URL, "k", "o", secureClient).getApp(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}
	if gotPath != "/v2/orgs/o/gdn/apps/app-1" {
		t.Errorf("path = %q", gotPath)
	}
	if app.SeatType != "LINUX" || len(app.InstanceNames) != 2 {
		t.Errorf("settings not parsed: %+v", app)
	}
	if f := app.ApplicationFeatures; f.MaxBitRate == nil || *f.MaxBitRate != 200 || len(f.DisplayMode) != 1 {
		t.Errorf("features not parsed: %+v", app.ApplicationFeatures)
	}
	if app.VersionCount != 2 {
		t.Errorf("VersionCount = %d, want 2 (from the server field, not len(Versions))", app.VersionCount)
	}
	ver := app.defaultVersion()
	if ver == nil || ver.ID != "ver-2" {
		t.Fatalf("defaultVersion = %+v, want ver-2", ver)
	}
	if len(ver.Artifacts) != 1 || ver.Artifacts[0].Name != "my-app-1" {
		t.Errorf("artifacts not parsed: %+v", ver.Artifacts)
	}
}

// The app payload embeds only the current version, so listVersions must read
// the full list from the dedicated versions endpoint while still taking the
// default id from the app itself.
func TestListVersionsFetchesFullList(t *testing.T) {
	var gotApp, gotVersions bool
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/orgs/o/gdn/apps/app-1":
			gotApp = true
			writeJSON(w, `{"id":"app-1","defaultVersionId":"ver-2","versionCount":2,"versions":[{"id":"ver-2","versionName":"1.1.0"}]}`)
		case "/v2/orgs/o/gdn/apps/app-1/versions":
			gotVersions = true
			writeJSON(w, `[{"id":"ver-1","versionName":"1.0.0"},{"id":"ver-2","versionName":"1.1.0"}]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	versions, defaultID, err := newPortal(srv.URL, "k", "o", secureClient).listVersions(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("listVersions: %v", err)
	}
	if !gotApp || !gotVersions {
		t.Errorf("listVersions should read the app (default id) and the versions endpoint (full list); got app=%t versions=%t", gotApp, gotVersions)
	}
	if defaultID != "ver-2" {
		t.Errorf("default id = %q, want ver-2", defaultID)
	}
	if len(versions) != 2 || versions[0].ID != "ver-1" || versions[1].ID != "ver-2" {
		t.Errorf("versions = %+v, want ver-1 and ver-2", versions)
	}
}

func TestGetAppVersionsAcceptsResponseEnvelope(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"versions":[{"id":"ver-1","versionName":"1.0.0"},{"id":"ver-2","versionName":"1.1.0"}]}`)
	}))
	defer srv.Close()

	versions, err := newPortal(srv.URL, "k", "o", secureClient).getAppVersions(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("getAppVersions: %v", err)
	}
	if len(versions) != 2 || versions[0].ID != "ver-1" || versions[1].ID != "ver-2" {
		t.Errorf("versions = %+v, want ver-1 and ver-2", versions)
	}
}

func TestGetAppVersionsRejectsEnvelopeWithoutVersions(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{}`)
	}))
	defer srv.Close()

	_, err := newPortal(srv.URL, "k", "o", secureClient).getAppVersions(context.Background(), "app-1")
	if err == nil || !strings.Contains(err.Error(), "missing versions") {
		t.Fatalf("getAppVersions error = %v, want missing versions", err)
	}
}

func TestDefaultVersionFallback(t *testing.T) {
	// No matching default id -> first version.
	a := gdnApp{DefaultVersionID: "missing", Versions: []gdnVersion{{ID: "v1"}, {ID: "v2"}}}
	if got := a.defaultVersion(); got == nil || got.ID != "v1" {
		t.Errorf("got %+v, want v1", got)
	}
	// No versions -> nil.
	if got := (&gdnApp{}).defaultVersion(); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestErrorPropagation(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"denied"}`))
	}))
	defer srv.Close()

	if _, err := newPortal(srv.URL, "k", "o", secureClient).listApps(context.Background()); err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestResolveInstanceNames(t *testing.T) {
	serve := func(t *testing.T, status int, body string) *portalClient {
		srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v2/gdn/apps/create-config" {
				t.Errorf("path = %q, want /v2/gdn/apps/create-config", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return newPortal(srv.URL, "k", "o", secureClient)
	}
	const offered = `{"allowedInstanceNames":["NVIDIA L40","NVIDIA L40G"]}`
	const emptyCatalogue = `{"allowedInstanceNames":[]}`
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		status    int
		body      string
		requested []string
		want      []string
		wantErr   string
	}{
		{name: "defaults to every offered name", status: 200, body: offered, want: []string{"NVIDIA L40", "NVIDIA L40G"}},
		{name: "accepts offered names", status: 200, body: offered, requested: []string{"NVIDIA L40G"}, want: []string{"NVIDIA L40G"}},
		{name: "rejects unknown names", status: 200, body: offered, requested: []string{"NVIDIA L40", "NVIDIA L10"}, wantErr: `unknown instance name(s) "NVIDIA L10"; GDN currently offers: NVIDIA L40, NVIDIA L40G`},
		{name: "empty catalogue sends requested names unchecked", status: 200, body: emptyCatalogue, requested: []string{"NVIDIA L10"}, want: []string{"NVIDIA L10"}},
		{name: "empty catalogue has no default", status: 200, body: emptyCatalogue, wantErr: "GDN offers no instance names right now"},
		{name: "unreadable config sends requested names unchecked", status: 403, body: `{}`, requested: []string{"NVIDIA L10"}, want: []string{"NVIDIA L10"}},
		{name: "unreadable config has no default", status: 403, body: `{}`, wantErr: "pass --instances"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serve(t, tc.status, tc.body).resolveInstanceNames(ctx, tc.requested)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveInstanceNames: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// GDN fills every applicationFeatures field a create request leaves out with
// its own default, so the request carries only what the caller set.
func TestCreateAppReqSendsOnlySetFeatures(t *testing.T) {
	without, err := json.Marshal(createAppReq{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "applicationFeatures") {
		t.Errorf("a request with no features carries applicationFeatures: %s", without)
	}

	rate := uint64(50)
	with, err := json.Marshal(createAppReq{Name: "a", ApplicationFeatures: &appFeature{MaxBitRate: &rate}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with), `"applicationFeatures":{"MAX_BIT_RATE":50}`) {
		t.Errorf("want only MAX_BIT_RATE in applicationFeatures, got %s", with)
	}
}

func TestAppFeatureSummary(t *testing.T) {
	var f appFeature
	if err := json.Unmarshal([]byte(`{"DISPLAY_MODE":[{"MAX_RESOLUTION":"1080p","MAX_FPS":60}],"MAX_BIT_DEPTH":10,"SUPPORTED_CHROMA_FORMAT":"YUV444","QOS_POLICY":"LATENCY","MAX_BIT_RATE":100,"TOUCH_SUPPORT":false,"IDLE_TIMEOUT":300,"SESSION_TIME_LIMIT":28800,"INSTANCE_REUSE_POLICY":"SECURITY_OPTIMIZED"}`), &f); err != nil {
		t.Fatal(err)
	}
	want := "1080p@60, 10-bit, YUV444, QoS LATENCY, 100 Mbps, touch false, idle 300s, session 28800s, reuse SECURITY_OPTIMIZED"
	if got := f.summary(); got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}
	if got := (appFeature{}).summary(); got != "" {
		t.Errorf("empty summary = %q, want empty", got)
	}
}

func TestValidateFeatures(t *testing.T) {
	for _, tc := range []struct {
		name, features, wantErr string
	}{
		{name: "neither policy", features: `{}`},
		{name: "full transport", features: `{"TRANSPORT_POLICY":{"POLICY":"RELAY","RELAY_PROTOCOL":"TCP","RELAY_LOCATION":"CSP"}}`},
		{name: "partial transport", features: `{"TRANSPORT_POLICY":{"POLICY":"RELAY"}}`, wantErr: "TRANSPORT_POLICY needs POLICY, RELAY_PROTOCOL and RELAY_LOCATION"},
		{name: "coverage ALL", features: `{"COVERAGE_AREA_POLICY":{"POLICY":"ALL"}}`},
		{name: "coverage CUSTOM with areas", features: `{"COVERAGE_AREA_POLICY":{"POLICY":"CUSTOM","COVERAGE_AREAS":["us-north-east"]}}`},
		{name: "coverage without POLICY", features: `{"COVERAGE_AREA_POLICY":{"COVERAGE_AREAS":["us-north-east"]}}`, wantErr: "COVERAGE_AREA_POLICY needs POLICY"},
		{name: "coverage CUSTOM without areas", features: `{"COVERAGE_AREA_POLICY":{"POLICY":"CUSTOM"}}`, wantErr: "POLICY CUSTOM needs COVERAGE_AREAS"},
		{name: "coverage unknown policy", features: `{"COVERAGE_AREA_POLICY":{"POLICY":"SOME"}}`, wantErr: `COVERAGE_AREA_POLICY.POLICY must be one of ALL, CUSTOM, got "SOME"`},
		{name: "transport CUSTOMER location", features: `{"TRANSPORT_POLICY":{"POLICY":"RELAY","RELAY_PROTOCOL":"TCP","RELAY_LOCATION":"CUSTOMER"}}`, wantErr: `TRANSPORT_POLICY.RELAY_LOCATION must be one of CSP, ZONE, got "CUSTOMER"`},
		{name: "transport unknown protocol", features: `{"TRANSPORT_POLICY":{"POLICY":"RELAY","RELAY_PROTOCOL":"QUIC","RELAY_LOCATION":"CSP"}}`, wantErr: "TRANSPORT_POLICY.RELAY_PROTOCOL must be one of ALL, UDP, TCP"},
		{name: "valid enums", features: `{"MAX_BIT_DEPTH":10,"SUPPORTED_CHROMA_FORMAT":"YUV420","QOS_POLICY":"QUALITY","INSTANCE_REUSE_POLICY":"BALANCED"}`},
		{name: "bit depth", features: `{"MAX_BIT_DEPTH":7}`, wantErr: "MAX_BIT_DEPTH must be 8 or 10, got 7"},
		{name: "chroma", features: `{"SUPPORTED_CHROMA_FORMAT":"BANANA"}`, wantErr: `SUPPORTED_CHROMA_FORMAT must be one of YUV444, YUV420, got "BANANA"`},
		{name: "qos", features: `{"QOS_POLICY":"NOPE"}`, wantErr: "QOS_POLICY must be one of QUALITY, LATENCY"},
		{name: "reuse", features: `{"INSTANCE_REUSE_POLICY":"WHATEVER"}`, wantErr: "INSTANCE_REUSE_POLICY must be one of PERFORMANCE_OPTIMIZED, BALANCED, SECURITY_OPTIMIZED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f appFeature
			if err := json.Unmarshal([]byte(tc.features), &f); err != nil {
				t.Fatal(err)
			}
			err := f.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validate = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestPoliciesRoundTripWhole(t *testing.T) {
	in := `{"TRANSPORT_POLICY":{"POLICY":"RELAY","RELAY_PROTOCOL":"TCP","RELAY_LOCATION":"ZONE"},"COVERAGE_AREA_POLICY":{"POLICY":"CUSTOM","COVERAGE_AREAS":["us-north-east","eu-west"]}}`
	var f appFeature
	if err := json.Unmarshal([]byte(in), &f); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("round trip changed the policies:\n got %s\nwant %s", out, in)
	}
	if got, want := f.summary(), "transport RELAY/TCP/ZONE, coverage CUSTOM (us-north-east, eu-west)"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
