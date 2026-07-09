// Package status_test exercises the status package in black-box style.
// All tests use httptest.Server and t.TempDir — no real network.
package status_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jesusrobot0/clauchy/internal/cache"
	"github.com/jesusrobot0/clauchy/internal/status"
)

// ----- fixtures -----

// fullSummary is the real Atlassian Statuspage shape with a Claude Code component.
const fullSummary = `{
	"page": {
		"id": "kf0d3vhpb0s8",
		"name": "Anthropic",
		"url": "https://status.claude.com",
		"updated_at": "2026-07-09T00:00:00Z"
	},
	"status": {
		"indicator": "none",
		"description": "All Systems Operational"
	},
	"components": [
		{"name": "claude.ai", "status": "operational"},
		{"name": "Claude Code", "status": "operational"},
		{"name": "API", "status": "operational"}
	]
}`

// summaryNoClaudeCode has no component named "Claude Code".
const summaryNoClaudeCode = `{
	"page": {"id": "x", "name": "Anthropic", "url": "https://status.claude.com", "updated_at": "2026-07-09T00:00:00Z"},
	"status": {"indicator": "minor", "description": "Minor Service Outage"},
	"components": [
		{"name": "claude.ai", "status": "degraded_performance"}
	]
}`

// summaryUnknownIndicator has an unrecognised indicator value.
const summaryUnknownIndicator = `{
	"page": {"id": "x", "name": "Anthropic", "url": "https://status.claude.com", "updated_at": "2026-07-09T00:00:00Z"},
	"status": {"indicator": "investigating", "description": "Something is happening"},
	"components": []
}`

// summaryClaudeCodeDegraded has Claude Code in degraded_performance.
const summaryClaudeCodeDegraded = `{
	"page": {"id": "x", "name": "Anthropic", "url": "https://status.claude.com", "updated_at": "2026-07-09T00:00:00Z"},
	"status": {"indicator": "minor", "description": "Minor issues"},
	"components": [
		{"name": "Claude Code", "status": "degraded_performance"}
	]
}`

// summaryClaudeCodeCritical has Claude Code in major_outage.
const summaryClaudeCodeCritical = `{
	"page": {"id": "x", "name": "Anthropic", "url": "https://status.claude.com", "updated_at": "2026-07-09T00:00:00Z"},
	"status": {"indicator": "major", "description": "Major outage"},
	"components": [
		{"name": "Claude Code", "status": "major_outage"}
	]
}`

// ----- helpers -----

// serveSummary starts an httptest server that returns body for GET
// /api/v2/summary.json.
func serveSummary(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/summary.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
}

// buildCachePayload returns a status.json byte slice with the given cachedAt.
func buildCachePayload(cachedAt time.Time, indicator, claudeCode string) []byte {
	return []byte(fmt.Sprintf(`{
		"cachedAt": %q,
		"indicator": %q,
		"description": "All Systems Operational",
		"claudeCode": %q
	}`, cachedAt.UTC().Format(time.RFC3339), indicator, claudeCode))
}

// cachedFast wraps status.Cached with a 50ms lock timeout to avoid slow tests.
func cachedFast(ctx context.Context, c *cache.Cache, h *http.Client, baseURL string, now func() time.Time) (status.Status, error) {
	return status.Cached(ctx, c, h, baseURL, now, 50*time.Millisecond)
}

// ----- Fetch tests -----

func TestFetch_FullSummary_ParsesCorrectly(t *testing.T) {
	t.Parallel()

	ts := serveSummary(t, fullSummary)
	defer ts.Close()

	got, err := status.Fetch(context.Background(), ts.Client(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	if got.Indicator != "none" {
		t.Errorf("Indicator = %q, want %q", got.Indicator, "none")
	}
	if got.Description != "All Systems Operational" {
		t.Errorf("Description = %q, want %q", got.Description, "All Systems Operational")
	}
	if got.ClaudeCode != "operational" {
		t.Errorf("ClaudeCode = %q, want %q", got.ClaudeCode, "operational")
	}
	if !got.CachedAt.IsZero() {
		t.Errorf("CachedAt should be zero from Fetch, got %v", got.CachedAt)
	}
	if got.Stale {
		t.Errorf("Stale should be false from Fetch")
	}
}

func TestFetch_NoClaudeCodeComponent_EmptyString(t *testing.T) {
	t.Parallel()

	ts := serveSummary(t, summaryNoClaudeCode)
	defer ts.Close()

	got, err := status.Fetch(context.Background(), ts.Client(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	if got.ClaudeCode != "" {
		t.Errorf("ClaudeCode = %q, want empty string when component absent", got.ClaudeCode)
	}
	if got.Indicator != "minor" {
		t.Errorf("Indicator = %q, want %q", got.Indicator, "minor")
	}
}

func TestFetch_UnknownIndicator_PassesThrough(t *testing.T) {
	t.Parallel()

	ts := serveSummary(t, summaryUnknownIndicator)
	defer ts.Close()

	got, err := status.Fetch(context.Background(), ts.Client(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	if got.Indicator != "investigating" {
		t.Errorf("Indicator = %q, want %q (unknown passed through)", got.Indicator, "investigating")
	}
}

func TestFetch_Non200_ReturnsErrTransient(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	_, err := status.Fetch(context.Background(), ts.Client(), ts.URL)
	if err == nil {
		t.Fatal("Fetch returned nil error for 503")
	}
	if !isErrTransient(err) {
		t.Errorf("expected ErrTransient, got %v", err)
	}
}

func TestFetch_TransportError_ReturnsErrTransient(t *testing.T) {
	t.Parallel()

	// Use a closed server to force a transport error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	ts.Close() // close immediately

	_, err := status.Fetch(context.Background(), ts.Client(), ts.URL)
	if err == nil {
		t.Fatal("Fetch returned nil error for closed server")
	}
	if !isErrTransient(err) {
		t.Errorf("expected ErrTransient, got %v", err)
	}
}

func TestFetch_BadJSON_ReturnsErrTransient(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{not valid json`)
	}))
	defer ts.Close()

	_, err := status.Fetch(context.Background(), ts.Client(), ts.URL)
	if err == nil {
		t.Fatal("Fetch returned nil error for bad JSON")
	}
	if !isErrTransient(err) {
		t.Errorf("expected ErrTransient, got %v", err)
	}
}

// isErrTransient checks using errors.Is on the sentinel.
func isErrTransient(err error) bool {
	return errors.Is(err, status.ErrTransient)
}

// ----- Cached tests -----

func TestCached_FreshHit_ReturnsCachedData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	// Pre-seed cache with fresh data (5s old).
	payload := buildCachePayload(now.Add(-5*time.Second), "none", "operational")
	if err := c.Write("status.json", payload); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Server should NOT be called.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called on fresh cache hit")
	}))
	defer ts.Close()

	got, err := cachedFast(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached returned error: %v", err)
	}
	if got.Stale {
		t.Errorf("Stale = true, want false for fresh cache")
	}
	if got.Indicator != "none" {
		t.Errorf("Indicator = %q, want %q", got.Indicator, "none")
	}
	if got.ClaudeCode != "operational" {
		t.Errorf("ClaudeCode = %q, want %q", got.ClaudeCode, "operational")
	}
}

func TestCached_TTLExpiry_RefetchesData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	// Seed cache with stale data (200s old = beyond 180s TTL).
	payload := buildCachePayload(now.Add(-200*time.Second), "none", "operational")
	if err := c.Write("status.json", payload); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	fetchCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fullSummary)
	}))
	defer ts.Close()

	got, err := cachedFast(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached returned error: %v", err)
	}
	if fetchCount == 0 {
		t.Error("expected server to be called on TTL expiry")
	}
	if got.Stale {
		t.Errorf("Stale = true, want false for fresh fetch")
	}
	if got.Indicator != "none" {
		t.Errorf("Indicator = %q, want %q", got.Indicator, "none")
	}
}

func TestCached_TransportError_StaleWithinCeiling_ReturnsStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	// Seed with data that is beyond TTL (200s) but within 24h stale ceiling.
	payload := buildCachePayload(now.Add(-200*time.Second), "none", "operational")
	if err := c.Write("status.json", payload); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Closed server → transport error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	ts.Close()

	got, err := cachedFast(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached should return stale data, not error: %v", err)
	}
	if !got.Stale {
		t.Errorf("Stale = false, want true for stale fallback")
	}
	if got.Indicator != "none" {
		t.Errorf("Indicator = %q, want stale values", got.Indicator)
	}
}

func TestCached_TransportError_BeyondCeiling_ReturnsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	// Seed with data older than 24h.
	payload := buildCachePayload(now.Add(-25*time.Hour), "none", "operational")
	if err := c.Write("status.json", payload); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	ts.Close()

	got, err := cachedFast(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	if err == nil {
		t.Fatalf("expected error beyond stale ceiling, got Status=%+v", got)
	}
	if !errors.Is(err, status.ErrTransient) {
		t.Errorf("beyond-ceiling error = %v, want wrapped ErrTransient", err)
	}
}

func TestCached_NoCache_TransportError_ReturnsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	ts.Close()

	_, err := cachedFast(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	if err == nil {
		t.Fatal("expected error when no cache and transport fails")
	}
}

func TestCached_LockTimeout_FallsBackToStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	// Seed with data that is beyond TTL (200s) but within ceiling.
	payload := buildCachePayload(now.Add(-200*time.Second), "none", "operational")
	if err := c.Write("status.json", payload); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Acquire the lock ourselves so Cached can't get it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.WithLock(".status.lock", 2*time.Second, func() error {
			time.Sleep(200 * time.Millisecond) // hold the lock
			return nil
		})
	}()
	time.Sleep(10 * time.Millisecond) // give the goroutine time to acquire

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called on lock timeout path")
	}))
	defer ts.Close()

	got, err := cachedFast(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	<-done
	if err != nil {
		t.Fatalf("lock timeout should fall back to stale, got error: %v", err)
	}
	if !got.Stale {
		t.Errorf("Stale = false, want true on lock timeout fallback")
	}
}

// TestCached_LockTimeout_NoStale_ReturnsErrTransient pins the unified no-data
// contract: a lock timeout with NO usable stale data must return
// (Status{}, wrapped ErrTransient) — the same shape as a transient fetch
// failure with no stale data. Callers classify both with errors.Is.
func TestCached_LockTimeout_NoStale_ReturnsErrTransient(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	// No cache seeded: the lock-timeout path has nothing to fall back to.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.WithLock(".status.lock", 2*time.Second, func() error {
			time.Sleep(200 * time.Millisecond) // hold the lock
			return nil
		})
	}()
	time.Sleep(10 * time.Millisecond) // give the goroutine time to acquire

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called on lock timeout path")
	}))
	defer ts.Close()

	got, err := cachedFast(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	<-done
	if err == nil {
		t.Fatalf("expected wrapped ErrTransient on lock timeout with no stale data, got Status=%+v", got)
	}
	if !errors.Is(err, status.ErrTransient) {
		t.Errorf("lock-timeout-no-stale error = %v, want wrapped ErrTransient", err)
	}
}

// TestCached_DoubleCheckedRead_CompetitorRefreshed mirrors the limits pattern:
// a competitor refreshes the cache while we wait for the lock. After acquiring
// it, the double-check must find the fresh data and return it WITHOUT a second
// HTTP fetch.
func TestCached_DoubleCheckedRead_CompetitorRefreshed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent double-check test in short mode")
	}
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	// Stale entry so Cached enters the lock path.
	if err := c.Write("status.json", buildCachePayload(now.Add(-200*time.Second), "none", "operational")); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	fetchCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetchCalls++
		w.WriteHeader(http.StatusInternalServerError) // would cause ErrTransient if called
	}))
	defer ts.Close()

	// Competitor: grab the lock, write a fresh entry, hold briefly, release.
	lockAcquired := make(chan struct{})
	competitorDone := make(chan struct{})
	go func() {
		_ = c.WithLock(".status.lock", 10*time.Second, func() error {
			close(lockAcquired)
			_ = c.Write("status.json", buildCachePayload(now, "none", "operational"))
			time.Sleep(80 * time.Millisecond) // hold long enough for Cached() to queue
			return nil
		})
		close(competitorDone)
	}()
	<-lockAcquired

	got, err := status.Cached(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	<-competitorDone

	if err != nil {
		t.Fatalf("Cached returned error: %v", err)
	}
	if fetchCalls > 0 {
		t.Errorf("Cached made %d HTTP calls; want 0 (double-check must return competitor's fresh data)", fetchCalls)
	}
	if got.Stale {
		t.Error("Stale = true after double-check found fresh competitor data, want false")
	}
}

// TestCached_ConcurrentCallers_SingleFetch runs two callers concurrently on an
// expired cache and asserts the flock + double-checked read collapse them into
// a single upstream fetch, with both callers getting fresh (non-stale) data.
func TestCached_ConcurrentCallers_SingleFetch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent two-caller test in short mode")
	}
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	// Expired entry so both callers want a refresh.
	if err := c.Write("status.json", buildCachePayload(now.Add(-200*time.Second), "none", "operational")); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var mu sync.Mutex
	fetchCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetchCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fullSummary)
	}))
	defer ts.Close()

	var wg sync.WaitGroup
	results := make([]status.Status, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = status.Cached(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
		}(i)
	}
	wg.Wait()

	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d returned error: %v", i, errs[i])
		}
		if results[i].Stale {
			t.Errorf("caller %d got Stale data, want fresh", i)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if fetchCalls != 1 {
		t.Errorf("upstream fetches = %d, want exactly 1 (loser must reuse winner's data)", fetchCalls)
	}
}

// ----- Worst tests -----

func TestWorst_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		indicator string
		cc        string // ClaudeCode component status
		want      string
	}{
		{
			name:      "all operational",
			indicator: "none",
			cc:        "operational",
			want:      "operational",
		},
		{
			name:      "indicator minor, cc operational",
			indicator: "minor",
			cc:        "operational",
			want:      "minor",
		},
		{
			name:      "indicator none, cc degraded_performance → minor",
			indicator: "none",
			cc:        "degraded_performance",
			want:      "minor",
		},
		{
			name:      "indicator minor, cc partial_outage → major",
			indicator: "minor",
			cc:        "partial_outage",
			want:      "major",
		},
		{
			name:      "indicator major, cc operational → major",
			indicator: "major",
			cc:        "operational",
			want:      "major",
		},
		{
			name:      "indicator none, cc major_outage → critical",
			indicator: "none",
			cc:        "major_outage",
			want:      "critical",
		},
		{
			name:      "indicator critical, cc operational → critical",
			indicator: "critical",
			cc:        "operational",
			want:      "critical",
		},
		{
			name:      "indicator major, cc major_outage → critical",
			indicator: "major",
			cc:        "major_outage",
			want:      "critical",
		},
		{
			name:      "empty ClaudeCode (component absent)",
			indicator: "minor",
			cc:        "",
			want:      "minor",
		},
		{
			name:      "all none, empty cc",
			indicator: "none",
			cc:        "",
			want:      "operational",
		},
		{
			name:      "unknown indicator passes through as minor",
			indicator: "investigating",
			cc:        "operational",
			want:      "minor", // unknown maps to minor (non-operational but not major/critical)
		},
		{
			// Zero-value Status (error fallback / absent data) must NOT read as
			// an incident: an empty indicator means "no data", not "unknown".
			name:      "zero-value Status (empty indicator, empty cc)",
			indicator: "",
			cc:        "",
			want:      "operational",
		},
		{
			// under_maintenance is deliberately mapped to minor: planned
			// maintenance still reduces availability (worth surfacing) but is
			// not an alarm-grade outage.
			name:      "cc under_maintenance → minor",
			indicator: "none",
			cc:        "under_maintenance",
			want:      "minor",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := status.Status{Indicator: tc.indicator, ClaudeCode: tc.cc}
			got := status.Worst(s)
			if got != tc.want {
				t.Errorf("Worst(%q, %q) = %q, want %q", tc.indicator, tc.cc, got, tc.want)
			}
		})
	}
}

// ----- HumanLabel tests -----

// TestHumanLabel_TableDriven pins the single human-label derivation shared by
// the dashboard footer and the waybar tooltip:
//   - ClaudeCode non-empty AND != "operational" → "Claude Code: " + humanized
//     component status (underscores become spaces).
//   - Otherwise → the page Description verbatim (no "Claude Code:" prefix —
//     the incident is elsewhere, so naming Claude Code would be a lie).
func TestHumanLabel_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		st   status.Status
		want string
	}{
		{
			name: "claude code partial outage → humanized with prefix",
			st:   status.Status{Indicator: "minor", ClaudeCode: "partial_outage", Description: "Partial outage"},
			want: "Claude Code: partial outage",
		},
		{
			name: "claude code degraded performance → humanized with prefix",
			st:   status.Status{Indicator: "minor", ClaudeCode: "degraded_performance", Description: "Minor issues"},
			want: "Claude Code: degraded performance",
		},
		{
			name: "claude code operational during non-Claude-Code incident → description, no prefix",
			st:   status.Status{Indicator: "minor", ClaudeCode: "operational", Description: "Elevated errors on claude.ai"},
			want: "Elevated errors on claude.ai",
		},
		{
			name: "component absent → description, no prefix",
			st:   status.Status{Indicator: "minor", ClaudeCode: "", Description: "Minor Service Outage"},
			want: "Minor Service Outage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := status.HumanLabel(tc.st); got != tc.want {
				t.Errorf("HumanLabel(%+v) = %q, want %q", tc.st, got, tc.want)
			}
		})
	}
}

// TestCached_WritesCachePayload verifies the cache file is written after a
// successful fetch and is valid JSON with a cachedAt field.
func TestCached_WritesCachePayload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	now := time.Now()

	ts := serveSummary(t, fullSummary)
	defer ts.Close()

	_, err := cachedFast(context.Background(), c, ts.Client(), ts.URL, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached returned error: %v", err)
	}

	raw, err := c.Read("status.json")
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	var payload struct {
		CachedAt time.Time `json:"cachedAt"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("cache payload is not valid JSON: %v", err)
	}
	if payload.CachedAt.IsZero() {
		t.Error("cachedAt in cache payload is zero")
	}
}
