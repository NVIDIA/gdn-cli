// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const retryMaxAttempts = 10

// jsonRequest is one JSON-over-HTTP call: which client sends it, where, what
// body goes out, and where the response is decoded. Bundled into a struct
// because it is passed down through the retry loop into each attempt.
type jsonRequest struct {
	hc     *http.Client
	apiKey string
	method string
	url    string
	in     any
	out    any
}

// httpJSON is shared HTTP transport for NGC and GDN clients.
func httpJSON(ctx context.Context, hc *http.Client, apiKey, method, url string, in, out any) error {
	r := jsonRequest{hc: hc, apiKey: apiKey, method: method, url: url, in: in, out: out}
	return httpJSONRetry(ctx, r, retryPolicy{})
}

type retryLogger func(format string, args ...any)

// retryPolicy carries the cross-request concerns of httpJSONRetry: where to
// report retries, and the shared throttle that reacts to server pushback. The
// zero value logs nothing and applies no throttling.
type retryPolicy struct {
	log      retryLogger
	throttle *apiThrottle
}

// httpJSONRetry calls httpJSON with exponential backoff on 429/502/503. Backoff
// alone only slows the one request that failed, so 429s and 503s also feed the
// shared throttle, which paces every other worker.
func httpJSONRetry(ctx context.Context, r jsonRequest, p retryPolicy) error {
	var bodyBytes []byte
	if r.in != nil {
		var err error
		bodyBytes, err = json.Marshal(r.in)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}

	for attempt := 0; ; attempt++ {
		if err := p.throttle.wait(ctx); err != nil {
			return err
		}
		res := r.attempt(ctx, bodyBytes, p)
		// Out of attempts is the same as a settled answer: hand back whatever
		// the last one failed with rather than sleeping and trying again.
		if res.settled || attempt >= retryMaxAttempts-1 {
			return res.err
		}
		if err := waitRetry(ctx, attempt, res.retryAfter, p.log, res.logFormat, res.logArgs...); err != nil {
			return err
		}
	}
}

// attemptResult is what one attempt decided. settled means stop and return err
// (nil on success); otherwise the caller may retry after retryAfter, reporting
// logFormat/logArgs as the reason.
type attemptResult struct {
	settled    bool
	err        error
	retryAfter time.Duration
	logFormat  string
	logArgs    []any
}

// attempt makes one request and classifies the outcome. It never sleeps; the
// backoff belongs to the caller's loop, which owns the attempt count.
func (r jsonRequest) attempt(ctx context.Context, bodyBytes []byte, p retryPolicy) attemptResult {
	var body io.Reader
	if bodyBytes != nil {
		body = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		return attemptResult{settled: true, err: err}
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Accept", "application/json")
	if r.in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.hc.Do(req)
	if err != nil {
		// A cancelled context will not be any healthier next attempt.
		if ctx.Err() != nil {
			return attemptResult{settled: true, err: err}
		}
		return attemptResult{err: err, logFormat: "%s %s: %v", logArgs: []any{r.method, r.url, err}}
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	switch {
	case isPushbackStatus(resp.StatusCode):
		p.throttle.pushback(retryAfter)
	case !isRetryableStatus(resp.StatusCode):
		// Any definitive answer, 409 included, is evidence the server is not
		// overloaded. See apiThrottle.served.
		p.throttle.served()
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return attemptResult{settled: true, err: r.decode(raw)}
	}

	err = fmt.Errorf("%s %s -> %s: %s", r.method, r.url, resp.Status, strings.TrimSpace(string(raw)))
	if !isRetryableStatus(resp.StatusCode) {
		return attemptResult{settled: true, err: err}
	}
	return attemptResult{err: err, retryAfter: retryAfter, logFormat: "%s", logArgs: []any{err}}
}

// decode unmarshals a successful response body, if the caller wanted one.
func (r jsonRequest) decode(raw []byte) error {
	if r.out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, r.out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func waitRetry(ctx context.Context, attempt int, retryAfter time.Duration, log retryLogger, format string, args ...any) error {
	delay := retryDelay(attempt, retryAfter)
	if log != nil {
		log(format+" — retrying in %s (attempt %d/%d)", append(args, delay, attempt+1, retryMaxAttempts)...)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func isRetryableStatus(code int) bool {
	return code == 429 || code == 502 || code == 503
}

// isPushbackStatus reports whether a status means "you are sending too much",
// as opposed to a one-off server fault. 502 is excluded: a bad gateway says
// nothing about our request rate, so throttling every worker over one would be
// an overreaction.
func isPushbackStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// parseRetryAfter handles both forms RFC 9110 allows: delay-seconds and an
// HTTP-date. Ignoring the date form would silently discard the server's own
// guidance and fall back to guessing with exponential backoff.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

func retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := time.Second << min(attempt, 6)
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	jitter := time.Duration(rand.Int64N(int64(d / 4)))
	return d + jitter
}

// isConflict is true for 409 / already-exists (treated as success).
func isConflict(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "409") || strings.Contains(s, "conflict") || strings.Contains(s, "already exists")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
