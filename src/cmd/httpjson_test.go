// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestHTTPJSONRetry429(t *testing.T) {
	t.Parallel()
	var attempts int32
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"statusCode":"TOO_MANY_REQUESTS"}`))
			return
		}
		writeJSON(w, `{"ok":true}`)
	}))
	defer srv.Close()

	var out struct {
		OK bool `json:"ok"`
	}
	req := jsonRequest{hc: secureClient, apiKey: "tok", method: http.MethodPost, url: srv.URL, in: map[string]any{}, out: &out}
	if err := httpJSONRetry(context.Background(), req, retryPolicy{}); err != nil {
		t.Fatalf("httpJSONRetry: %v", err)
	}
	if !out.OK {
		t.Fatal("expected ok response")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestHTTPJSONRetryNonRetryable(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	req := jsonRequest{hc: secureClient, apiKey: "tok", method: http.MethodPost, url: srv.URL, in: map[string]any{}}
	err := httpJSONRetry(context.Background(), req, retryPolicy{})
	if err == nil {
		t.Fatal("expected error")
	}
	if isConflict(err) {
		t.Fatal("400 should not be treated as conflict")
	}
}

func TestIsRetryableStatus(t *testing.T) {
	for _, code := range []int{429, 502, 503} {
		if !isRetryableStatus(code) {
			t.Errorf("status %d should be retryable", code)
		}
	}
	for _, code := range []int{400, 401, 404, 409, 500} {
		if isRetryableStatus(code) {
			t.Errorf("status %d should not be retryable", code)
		}
	}
}
