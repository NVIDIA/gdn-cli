// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestThrottleIdleUntilPushback checks the throttle costs nothing on the happy
// path: no pushback means no pacing and no delay.
func TestThrottleIdleUntilPushback(t *testing.T) {
	t.Parallel()
	var th apiThrottle
	start := time.Now()
	for range 100 {
		if err := th.wait(context.Background()); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("100 unthrottled waits took %v, want ~0", elapsed)
	}
	if got := th.currentInterval(); got != 0 {
		t.Errorf("interval = %v, want 0", got)
	}
}

// TestThrottleNilIsNoOp guards the nil-receiver contract, which lets callers
// that do not need throttling pass nil.
func TestThrottleNilIsNoOp(t *testing.T) {
	t.Parallel()
	var th *apiThrottle
	if err := th.wait(context.Background()); err != nil {
		t.Fatalf("wait on nil: %v", err)
	}
	th.pushback(time.Second)
	th.served()
	if got := th.currentInterval(); got != 0 {
		t.Errorf("interval = %v, want 0", got)
	}
}

// TestThrottleBurstOf429sAdjustsOnce is the important one: dozens of workers can
// be in flight when the server starts refusing, and if each 429 doubled the
// interval the throttle would slam straight to its floor.
func TestThrottleBurstOf429sAdjustsOnce(t *testing.T) {
	t.Parallel()
	var th apiThrottle
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			th.pushback(0)
		}()
	}
	wg.Wait()

	if got := th.currentInterval(); got != throttleBaseInterval {
		t.Errorf("interval after a burst = %v, want %v (one adjustment)", got, throttleBaseInterval)
	}
}

// TestThrottleEscalatesAndRecovers walks the interval up under sustained
// pushback and back down once calls succeed again.
func TestThrottleEscalatesAndRecovers(t *testing.T) {
	t.Parallel()
	var th apiThrottle

	th.pushback(0)
	first := th.currentInterval()
	if first != throttleBaseInterval {
		t.Fatalf("first interval = %v, want %v", first, throttleBaseInterval)
	}

	// A second pushback inside the cooldown must not compound.
	th.pushback(0)
	if got := th.currentInterval(); got != first {
		t.Fatalf("interval = %v after pushback inside cooldown, want %v", got, first)
	}

	// Past the cooldown it should double.
	th.mu.Lock()
	th.lastDecrease = time.Now().Add(-2 * throttleDecreaseCooldown)
	th.mu.Unlock()
	th.pushback(0)
	if got, want := th.currentInterval(), first*2; got != want {
		t.Fatalf("escalated interval = %v, want %v", got, want)
	}

	// Successes halve it, then disengage it entirely.
	for range throttleRecoverAfter {
		th.served()
	}
	if got := th.currentInterval(); got != first {
		t.Fatalf("interval after recovery round = %v, want %v", got, first)
	}
	for range throttleRecoverAfter {
		th.served()
	}
	if got := th.currentInterval(); got != 0 {
		t.Errorf("interval = %v after full recovery, want 0 (disengaged)", got)
	}
}

// TestThrottleSpacesConcurrentWaiters checks that a throttled wait actually
// spreads callers out rather than releasing them together.
func TestThrottleSpacesConcurrentWaiters(t *testing.T) {
	t.Parallel()
	var th apiThrottle
	th.pushback(0) // engage pacing at the base interval

	const n = 5
	start := time.Now()
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = th.wait(context.Background())
		}()
	}
	wg.Wait()

	// n reserved slots span at least (n-1) intervals.
	wantMin := time.Duration(n-1) * throttleBaseInterval
	if elapsed := time.Since(start); elapsed < wantMin {
		t.Errorf("%d paced waits finished in %v, want at least %v", n, elapsed, wantMin)
	}
}

func TestThrottleWaitRespectsCancellation(t *testing.T) {
	t.Parallel()
	var th apiThrottle
	th.pushback(10 * time.Second) // long server-requested cooldown

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := th.wait(ctx); err != context.Canceled {
		t.Errorf("wait error = %v, want context.Canceled", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("delay-seconds: got %v, want 5s", got)
	}
	if got := parseRetryAfter("  7 "); got != 7*time.Second {
		t.Errorf("padded delay-seconds: got %v, want 7s", got)
	}
	for _, v := range []string{"", "0", "-3", "garbage"} {
		if got := parseRetryAfter(v); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", v, got)
		}
	}
	// HTTP-date form: previously ignored, so the server's guidance was lost.
	future := time.Now().UTC().Add(30 * time.Second).Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 31*time.Second {
		t.Errorf("HTTP-date: got %v, want ~30s", got)
	}
	past := time.Now().UTC().Add(-time.Minute).Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("past HTTP-date: got %v, want 0", got)
	}
}

// TestHTTPJSONRetryEngagesThrottleOn429 ties the pieces together: a 429 from the
// server must leave the shared throttle pacing subsequent requests.
func TestHTTPJSONRetryEngagesThrottleOn429(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"statusCode":"TOO_MANY_REQUESTS"}`))
			return
		}
		writeJSON(w, `{"ok":true}`)
	}))
	defer srv.Close()

	th := &apiThrottle{}
	var out struct {
		OK bool `json:"ok"`
	}
	req := jsonRequest{hc: secureClient, apiKey: "tok", method: http.MethodPost, url: srv.URL, in: map[string]any{}, out: &out}
	err := httpJSONRetry(context.Background(), req, retryPolicy{throttle: th})
	if err != nil {
		t.Fatalf("httpJSONRetry: %v", err)
	}
	if !out.OK {
		t.Error("expected the retry to succeed")
	}
	if got := th.currentInterval(); got != throttleBaseInterval {
		t.Errorf("throttle interval = %v, want %v after a 429", got, throttleBaseInterval)
	}
}

// TestHTTPJSONRetryRecoversOnConflict covers re-uploading a tree that is already
// in NGC: every file answers 409, and if only 2xx counted as recovery the
// throttle would ratchet to its floor and stay there for the whole run.
func TestHTTPJSONRetryRecoversOnConflict(t *testing.T) {
	t.Parallel()
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"statusCode":"CONFLICT"}`))
	}))
	defer srv.Close()

	th := &apiThrottle{}
	th.pushback(0) // engage pacing as a live 429 would
	if got := th.currentInterval(); got != throttleBaseInterval {
		t.Fatalf("setup: interval = %v, want %v", got, throttleBaseInterval)
	}

	for range throttleRecoverAfter {
		req := jsonRequest{hc: secureClient, apiKey: "tok", method: http.MethodPost, url: srv.URL, in: map[string]any{}}
		err := httpJSONRetry(context.Background(), req, retryPolicy{throttle: th})
		if err == nil {
			t.Fatal("expected the 409 to surface as an error")
		}
	}
	if got := th.currentInterval(); got != 0 {
		t.Errorf("interval = %v after %d conflicts, want 0 (409s must count as recovery)",
			got, throttleRecoverAfter)
	}
}

// TestHTTPJSONRetryNoThrottleOnBadGateway checks 502 is treated as a one-off
// fault rather than a rate signal.
func TestHTTPJSONRetryNoThrottleOnBadGateway(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeJSON(w, `{"ok":true}`)
	}))
	defer srv.Close()

	th := &apiThrottle{}
	req := jsonRequest{hc: secureClient, apiKey: "tok", method: http.MethodGet, url: srv.URL}
	err := httpJSONRetry(context.Background(), req, retryPolicy{throttle: th})
	if err != nil {
		t.Fatalf("httpJSONRetry: %v", err)
	}
	if got := th.currentInterval(); got != 0 {
		t.Errorf("throttle interval = %v, want 0 (502 is not a rate signal)", got)
	}
}
