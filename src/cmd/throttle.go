// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"sync"
	"time"
)

const (
	// throttleBaseInterval is the spacing applied the first time the server
	// pushes back. It has to be tight enough to restrain the worker pool it is
	// protecting: N workers at ~0.4s per NGC call already offer only N/0.4
	// requests/s, so a first step looser than that paces nothing at all. At 50ms
	// (20 req/s) Little's law puts ~8 requests in flight, which bites at any
	// worker count we ship.
	throttleBaseInterval = 50 * time.Millisecond
	// throttleMaxInterval floors the offered rate at ~4 requests/s. Reaching it
	// means the server is refusing sustained load, so going slower still beats
	// burning retry attempts.
	throttleMaxInterval = 250 * time.Millisecond
	// throttleDecreaseCooldown collapses a burst of 429s into one adjustment.
	// Dozens of workers can be in flight when the server starts refusing, and
	// letting each one double the interval would jump straight to the floor.
	throttleDecreaseCooldown = time.Second
	// throttleRecoverAfter is how many consecutive served responses halve the
	// interval.
	throttleRecoverAfter = 25
)

// apiThrottle paces NGC API requests across every worker. The per-request retry
// in httpJSONRetry backs off one request at a time, which is not enough on its
// own: with N workers all retrying independently, the aggregate offered rate
// barely drops and the burst that caused the 429 simply repeats a second later.
// This type turns server pushback into a shared signal, spacing requests
// globally (multiplicative increase) and easing off once calls succeed again
// (gradual decrease).
//
// It stays completely out of the way until the first pushback: while interval is
// zero, wait only observes any cooldown the server explicitly asked for.
//
// A nil *apiThrottle is valid and does nothing, so callers that do not need
// throttling can pass nil.
type apiThrottle struct {
	mu           sync.Mutex
	interval     time.Duration // 0 = unthrottled
	next         time.Time     // earliest instant the next request may go out
	lastDecrease time.Time     // when the interval last grew
	servedRun    int           // consecutive served responses since the last increase
}

// wait blocks until this request's paced slot arrives, reserving it so
// concurrent callers spread out instead of waking together.
func (t *apiThrottle) wait(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	now := time.Now()
	slot := t.next
	if t.interval > 0 {
		// Reserve a slot and advance the cursor for the next caller.
		if slot.Before(now) {
			slot = now
		}
		t.next = slot.Add(t.interval)
	}
	t.mu.Unlock()

	delay := time.Until(slot)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// pushback records that the server refused a request. retryAfter, when the
// server supplied one, holds every worker off for at least that long.
func (t *apiThrottle) pushback(retryAfter time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.servedRun = 0
	now := time.Now()
	// One adjustment per cooldown window, however many workers report a 429.
	if now.Sub(t.lastDecrease) >= throttleDecreaseCooldown {
		t.lastDecrease = now
		switch {
		case t.interval <= 0:
			t.interval = throttleBaseInterval
		case t.interval < throttleMaxInterval:
			t.interval = min(t.interval*2, throttleMaxInterval)
		}
	}
	if retryAfter > 0 {
		if until := now.Add(retryAfter); until.After(t.next) {
			t.next = until
		}
	}
}

// served records that the server gave a definitive answer, easing the pacing
// back off and disengaging it entirely once the interval drops below the base
// step.
//
// The caller decides what counts, and it is deliberately not "2xx": a 409 means
// the file is already in NGC, which is an ordinary outcome of re-running an
// upload and just as much proof the server is healthy as a 200. Recovering only
// on 2xx would leave a re-run — where nearly every response is a 409 — pinned at
// the throttle's floor from the first 429 onward.
func (t *apiThrottle) served() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.interval <= 0 {
		return
	}
	t.servedRun++
	if t.servedRun < throttleRecoverAfter {
		return
	}
	t.servedRun = 0
	if t.interval /= 2; t.interval < throttleBaseInterval {
		t.interval = 0
	}
}

// currentInterval reports the pacing in effect, for tests and diagnostics.
func (t *apiThrottle) currentInterval() time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.interval
}
