// Package limits_test exercises the limits package in black-box style.
// All tests use httptest.Server for the usage endpoint and t.TempDir for the
// cache directory — no real network, no real home directory.
package limits_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jesusrobot0/clauchy/internal/cache"
	"github.com/jesusrobot0/clauchy/internal/limits"
	"github.com/jesusrobot0/clauchy/internal/oauth"
)

// cached is a thin wrapper around limits.Cached that passes the test-friendly
// 50ms lock timeout so tests don't wait 3s for the default.
func cachedFast(ctx context.Context, c *cache.Cache, h *http.Client, url string, tok limits.TokenFunc, now func() time.Time) (limits.Usage, error) {
	return limits.Cached(ctx, c, h, url, tok, now, 50*time.Millisecond)
}

// ----- helpers -----

// validAPIResponse is the minimal JSON returned by a happy-path usage server.
const validAPIResponse = `{
	"five_hour":  {"utilization": 0.42, "resets_at": "2026-07-07T15:00:00Z"},
	"seven_day":  {"utilization": 0.18, "resets_at": "2026-07-14T00:00:00Z"}
}`

// fullAPIResponse includes all optional fields using the REAL API schema for limits[].
// limits[] entries use kind/scope.model.display_name/percent — NOT name/utilization.
const fullAPIResponse = `{
	"five_hour":  {"utilization": 0.42, "resets_at": "2026-07-07T15:00:00Z"},
	"seven_day":  {"utilization": 0.18, "resets_at": "2026-07-14T00:00:00Z"},
	"seven_day_sonnet": {"utilization": 0.10, "resets_at": "2026-07-14T00:00:00Z"},
	"limits": [
		{
			"kind": "weekly_scoped",
			"scope": {"model": {"display_name": "Fable"}},
			"percent": 86,
			"resets_at": "2026-07-14T00:00:00Z"
		}
	],
	"extra_usage": {"some_field": 42}
}`

// legacyShapeResponse simulates the OLD (wrong) API schema where limits[] entries
// carried name/utilization/resets_at fields. New code must degrade gracefully to
// zero model entries — no crash — because entries lack kind=="weekly_scoped".
const legacyShapeResponse = `{
	"five_hour":  {"utilization": 0.42, "resets_at": "2026-07-07T15:00:00Z"},
	"seven_day":  {"utilization": 0.18, "resets_at": "2026-07-14T00:00:00Z"},
	"limits": [
		{"name": "claude-opus-4-8", "utilization": 0.05, "resets_at": "2026-07-14T00:00:00Z"}
	]
}`

// buildCachePayload returns a usage.json byte slice with the given cachedAt.
// It uses the same five_hour/seven_day values as validAPIResponse.
func buildCachePayload(cachedAt time.Time) []byte {
	return []byte(fmt.Sprintf(`{
		"cachedAt": %q,
		"five_hour":  {"utilization": 0.42, "resets_at": "2026-07-07T15:00:00Z"},
		"seven_day":  {"utilization": 0.18, "resets_at": "2026-07-14T00:00:00Z"}
	}`, cachedAt.UTC().Format(time.RFC3339)))
}

// okTok is a TokenFunc that always succeeds.
func okTok() (string, error) { return "test-bearer-token", nil }

// ----- Fetch tests (T-2.4) -----

func TestFetch_HappyPath(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, validAPIResponse)
	}))
	defer ts.Close()

	ctx := context.Background()
	u, err := limits.Fetch(ctx, ts.Client(), "test-token", ts.URL)
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if u.FiveHour.Utilization != 0.42 {
		t.Errorf("FiveHour.Utilization = %v, want 0.42", u.FiveHour.Utilization)
	}
	if u.SevenDay.Utilization != 0.18 {
		t.Errorf("SevenDay.Utilization = %v, want 0.18", u.SevenDay.Utilization)
	}
	wantReset := time.Date(2026, 7, 7, 15, 0, 0, 0, time.UTC)
	if !u.FiveHour.ResetsAt.Equal(wantReset) {
		t.Errorf("FiveHour.ResetsAt = %v, want %v", u.FiveHour.ResetsAt, wantReset)
	}
}

func TestFetch_OptionalFields(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fullAPIResponse)
	}))
	defer ts.Close()

	ctx := context.Background()
	u, err := limits.Fetch(ctx, ts.Client(), "test-token", ts.URL)
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if u.SevenDaySonnet == nil {
		t.Error("SevenDaySonnet = nil, want non-nil")
	} else if u.SevenDaySonnet.Utilization != 0.10 {
		t.Errorf("SevenDaySonnet.Utilization = %v, want 0.10", u.SevenDaySonnet.Utilization)
	}
	if len(u.Models) != 1 {
		t.Errorf("Models len = %d, want 1", len(u.Models))
	} else {
		// Real schema: display_name "Fable", percent 86 (stored as Utilization).
		if u.Models[0].Name != "Fable" {
			t.Errorf("Models[0].Name = %q, want Fable (from scope.model.display_name)", u.Models[0].Name)
		}
		if u.Models[0].Utilization != 86 {
			t.Errorf("Models[0].Utilization = %v, want 86 (from percent field)", u.Models[0].Utilization)
		}
	}
	if u.Extra == nil {
		t.Error("Extra = nil, want non-nil when extra_usage is present")
	}
}

func TestFetch_RequestHeaders(t *testing.T) {
	// Both Authorization: Bearer <token> and anthropic-beta: oauth-2025-04-20 must be set.
	t.Parallel()

	var gotAuth, gotBeta string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, validAPIResponse)
	}))
	defer ts.Close()

	ctx := context.Background()
	if _, err := limits.Fetch(ctx, ts.Client(), "my-bearer-token", ts.URL); err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}

	if gotAuth != "Bearer my-bearer-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer my-bearer-token")
	}
	if gotBeta != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", gotBeta)
	}
}

func TestFetch_5xxReturnsErrTransient(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	_, err := limits.Fetch(context.Background(), ts.Client(), "tok", ts.URL)
	if !errors.Is(err, limits.ErrTransient) {
		t.Errorf("Fetch() error = %v, want ErrTransient", err)
	}
}

func TestFetch_ContextTimeoutReturnsErrTransient(t *testing.T) {
	t.Parallel()

	// Server that delays longer than the context deadline.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until client gives up.
		<-r.Context().Done()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := limits.Fetch(ctx, ts.Client(), "tok", ts.URL)
	if !errors.Is(err, limits.ErrTransient) {
		t.Errorf("Fetch() on timeout error = %v, want ErrTransient", err)
	}
}

// ----- Cached tests (T-2.5) -----

func TestCached_FreshCacheHitNoHTTP(t *testing.T) {
	// A cache entry whose age < 90s must be returned without any HTTP call.
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	// Write cache entry stamped 50s ago → age < 90s → should be a hit.
	if err := c.Write("usage.json", buildCachePayload(now.Add(-50*time.Second))); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() made an HTTP call on a fresh cache hit")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, okTok, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached() error: %v", err)
	}
	if u.Stale {
		t.Error("Usage.Stale = true on a fresh cache hit, want false")
	}
	if u.FiveHour.Utilization != 0.42 {
		t.Errorf("FiveHour.Utilization = %v, want 0.42", u.FiveHour.Utilization)
	}
}

func TestCached_StaleCacheTriggersRefetch(t *testing.T) {
	// A cache entry older than 90s must trigger a new fetch, and the new
	// cachedAt must be embedded in the refreshed payload.
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	// Write entry stamped 120s ago → stale.
	if err := c.Write("usage.json", buildCachePayload(now.Add(-120*time.Second))); err != nil {
		t.Fatal(err)
	}

	fetchCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCalls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, validAPIResponse)
	}))
	defer ts.Close()

	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, okTok, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached() error: %v", err)
	}
	if fetchCalls != 1 {
		t.Errorf("HTTP fetch calls = %d, want 1", fetchCalls)
	}
	if u.Stale {
		t.Error("Usage.Stale = true on a successful fresh fetch, want false")
	}
	// New cachedAt should be embedded in written payload (verify by reading cache).
	raw, err := c.Read("usage.json")
	if err != nil {
		t.Fatalf("Read() cache after Cached(): %v", err)
	}
	if len(raw) == 0 {
		t.Error("cache is empty after fetch")
	}
}

func TestCached_ErrTransientWithStale_ReturnsStaleValues(t *testing.T) {
	// ErrTransient from the fetch + stale cache ≤7d → return stale Usage{Stale:true}.
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	// Stale entry, but within the 7-day window.
	staleAt := now.Add(-2 * 24 * time.Hour) // 2 days old
	if err := c.Write("usage.json", buildCachePayload(staleAt)); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // → ErrTransient
	}))
	defer ts.Close()

	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, okTok, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached() error: %v", err)
	}
	if !u.Stale {
		t.Error("Usage.Stale = false on transient error with stale cache, want true")
	}
	if u.FiveHour.Utilization != 0.42 {
		t.Errorf("FiveHour.Utilization = %v, want 0.42 (stale values returned)", u.FiveHour.Utilization)
	}
}

func TestCached_ErrTransientNoCache_ReturnsZeroUsage(t *testing.T) {
	// ErrTransient + no usable cache (>7d) → (Usage{}, nil) — "Loading..." state.
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	// Cache entry older than 7 days → treat as miss.
	staleAt := now.Add(-8 * 24 * time.Hour)
	if err := c.Write("usage.json", buildCachePayload(staleAt)); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, okTok, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached() error = %v, want nil (zero Usage signals Loading...)", err)
	}
	if u.FiveHour.Utilization != 0 || u.SevenDay.Utilization != 0 {
		t.Errorf("Expected zero Usage on transient+>7d, got FiveHour=%v SevenDay=%v",
			u.FiveHour.Utilization, u.SevenDay.Utilization)
	}
}

func TestCached_TokErrRefreshRejected_WithStale_ReturnsStale(t *testing.T) {
	// oauth.ErrRefreshRejected from tok() + stale ≤7d → stale values, NOT "Run claude to log in".
	// Stale data is served and no error is returned.
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	if err := c.Write("usage.json", buildCachePayload(now.Add(-3*24*time.Hour))); err != nil {
		t.Fatal(err)
	}

	rejectedTok := func() (string, error) { return "", oauth.ErrRefreshRejected }

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() must not call the usage API when tok() fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, rejectedTok, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Cached() error = %v, want nil (stale values must be served)", err)
	}
	if !u.Stale {
		t.Error("Usage.Stale = false, want true (stale data served after refresh rejection)")
	}
	if u.FiveHour.Utilization != 0.42 {
		t.Errorf("FiveHour.Utilization = %v, want 0.42 (stale values)", u.FiveHour.Utilization)
	}
}

func TestCached_TokErrRefreshRejected_NoUsableCache_PropagatesErr(t *testing.T) {
	// oauth.ErrRefreshRejected + >7d stale → re-auth signal (error propagated).
	// This is the "persistent rejection past the stale window" case.
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	// Write entry older than 7 days → no usable stale data.
	if err := c.Write("usage.json", buildCachePayload(now.Add(-8*24*time.Hour))); err != nil {
		t.Fatal(err)
	}

	rejectedTok := func() (string, error) { return "", oauth.ErrRefreshRejected }

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() must not call the usage API when tok() fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	_, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, rejectedTok, func() time.Time { return now })
	if !errors.Is(err, oauth.ErrRefreshRejected) {
		t.Errorf("Cached() error = %v, want oauth.ErrRefreshRejected (re-auth signal)", err)
	}
}

func TestCached_LockTimeout_FallsBackToStale(t *testing.T) {
	// Lock contention past the try-lock deadline → ErrLockTimeout → fall back
	// to stale values. Uses an injectable 50ms lock timeout so the test runs
	// in milliseconds rather than the 3s wall-clock default.
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	// Write stale entry (so the lock path is entered).
	if err := c.Write("usage.json", buildCachePayload(now.Add(-120*time.Second))); err != nil {
		t.Fatal(err)
	}

	// Hold the fetch lock for longer than the injected 50ms try-lock budget.
	lockHeld := make(chan struct{})
	lockRelease := make(chan struct{})
	go func() {
		_ = c.WithLock(".fetch.lock", 5*time.Second, func() error {
			close(lockHeld)
			<-lockRelease
			return nil
		})
	}()
	<-lockHeld // wait until goroutine holds the lock

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() made HTTP call despite lock timeout")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	// Inject a 50ms lock timeout — fast test, no 3s wall-clock burn.
	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, okTok, func() time.Time { return now }, 50*time.Millisecond)
	close(lockRelease)

	if err != nil {
		t.Fatalf("Cached() error = %v, want nil (should fall back to stale)", err)
	}
	if !u.Stale {
		t.Error("Usage.Stale = false after lock timeout fallback, want true")
	}
}

func TestCached_DoubleCheckedRead_CompetitorRefreshed(t *testing.T) {
	// A competitor goroutine refreshes the cache while we're waiting for the lock.
	// After acquiring the lock, the double-check should find fresh data and return
	// it without making another HTTP call.
	if testing.Short() {
		t.Skip("skipping concurrent double-check test in short mode")
	}
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	// Write a stale entry so Cached() enters the lock path.
	if err := c.Write("usage.json", buildCachePayload(now.Add(-120*time.Second))); err != nil {
		t.Fatal(err)
	}

	fetchCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetchCalls++
		w.WriteHeader(http.StatusInternalServerError) // would cause ErrTransient if called
	}))
	defer ts.Close()

	// Competitor: grab the lock, write a fresh cache entry, sleep briefly, release.
	lockAcquired := make(chan struct{})
	competitorDone := make(chan struct{})
	go func() {
		_ = c.WithLock(".fetch.lock", 10*time.Second, func() error {
			close(lockAcquired)
			// Write a fresh entry to simulate a successful competitor fetch.
			_ = c.Write("usage.json", buildCachePayload(now))
			time.Sleep(80 * time.Millisecond) // hold long enough for Cached() to queue
			return nil
		})
		close(competitorDone)
	}()
	<-lockAcquired // ensure competitor holds the lock before we call Cached()

	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, okTok, func() time.Time { return now })
	<-competitorDone

	if err != nil {
		t.Fatalf("Cached() error: %v", err)
	}
	if fetchCalls > 0 {
		t.Errorf("Cached() made %d HTTP calls; want 0 (double-check must return competitor's fresh data)", fetchCalls)
	}
	if u.Stale {
		t.Error("Usage.Stale = true after double-check found fresh competitor data, want false")
	}
}

func TestCached_UsageJSONContainsNoTokenMaterial(t *testing.T) {
	// After a successful Cached() call, usage.json must contain no auth tokens.
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	const sensitiveToken = "sk-ant-secret-token"
	tok := func() (string, error) { return sensitiveToken, nil }

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, validAPIResponse)
	}))
	defer ts.Close()

	if _, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, tok, func() time.Time { return now }); err != nil {
		t.Fatalf("Cached() error: %v", err)
	}

	raw, err := c.Read("usage.json")
	if err != nil {
		t.Fatalf("Read() cache: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("usage.json is empty")
	}
	if containsString(raw, sensitiveToken) {
		t.Errorf("usage.json contains the bearer token %q — tokens must never be cached", sensitiveToken)
	}
}

// ----- Real-schema parsing tests (Bug 1 fix) -----

// TestFetch_LegacyShapeDegrades asserts that an old-format limits[] response
// (name/utilization fields, no kind field) produces zero model entries rather
// than a crash. Old entries lack kind=="weekly_scoped" so they are filtered out.
func TestFetch_LegacyShapeDegrades(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, legacyShapeResponse)
	}))
	defer ts.Close()

	ctx := context.Background()
	u, err := limits.Fetch(ctx, ts.Client(), "test-token", ts.URL)
	if err != nil {
		t.Fatalf("Fetch() error on legacy payload: %v", err)
	}
	// five_hour/seven_day must still parse correctly.
	if u.FiveHour.Utilization != 0.42 {
		t.Errorf("FiveHour.Utilization = %v, want 0.42", u.FiveHour.Utilization)
	}
	// Old-format limits[] entries have no kind field → all filtered → zero entries.
	if len(u.Models) != 0 {
		t.Errorf("Models len = %d, want 0 (legacy shape degrades gracefully)", len(u.Models))
	}
}

// TestFetch_SonnetDedup_WhenSevenDaySonnetPresent verifies that a limits[] entry
// with display_name=="Sonnet" is skipped when seven_day_sonnet is already present
// in the response (duplicate window guard from claudebar).
func TestFetch_SonnetDedup_WhenSevenDaySonnetPresent(t *testing.T) {
	t.Parallel()

	const body = `{
		"five_hour": {"utilization": 0.42, "resets_at": "2026-07-07T15:00:00Z"},
		"seven_day": {"utilization": 0.18, "resets_at": "2026-07-14T00:00:00Z"},
		"seven_day_sonnet": {"utilization": 0.10, "resets_at": "2026-07-14T00:00:00Z"},
		"limits": [
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "Sonnet"}},
			 "percent": 10, "resets_at": "2026-07-14T00:00:00Z"},
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "Opus"}},
			 "percent": 50, "resets_at": "2026-07-14T00:00:00Z"}
		]
	}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer ts.Close()

	ctx := context.Background()
	u, err := limits.Fetch(ctx, ts.Client(), "test-token", ts.URL)
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	// Sonnet skipped (deduped with seven_day_sonnet); Opus kept → exactly 1 entry.
	if len(u.Models) != 1 {
		t.Errorf("Models len = %d, want 1 (Sonnet deduped when seven_day_sonnet present)", len(u.Models))
	}
	if len(u.Models) == 1 && u.Models[0].Name != "Opus" {
		t.Errorf("Models[0].Name = %q, want Opus (Sonnet should have been skipped)", u.Models[0].Name)
	}
}

// TestFetch_CapAt4Entries verifies that more than 4 limits[] entries are capped
// at 4, so hostile payloads cannot grow the tooltip or burn CPU indefinitely.
func TestFetch_CapAt4Entries(t *testing.T) {
	t.Parallel()

	const body = `{
		"five_hour": {"utilization": 0.1, "resets_at": "2026-07-07T15:00:00Z"},
		"seven_day": {"utilization": 0.1, "resets_at": "2026-07-14T00:00:00Z"},
		"limits": [
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "A"}}, "percent": 10, "resets_at": "2026-07-14T00:00:00Z"},
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "B"}}, "percent": 20, "resets_at": "2026-07-14T00:00:00Z"},
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "C"}}, "percent": 30, "resets_at": "2026-07-14T00:00:00Z"},
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "D"}}, "percent": 40, "resets_at": "2026-07-14T00:00:00Z"},
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "E"}}, "percent": 50, "resets_at": "2026-07-14T00:00:00Z"}
		]
	}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer ts.Close()

	ctx := context.Background()
	u, err := limits.Fetch(ctx, ts.Client(), "test-token", ts.URL)
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if len(u.Models) > 4 {
		t.Errorf("Models len = %d, want ≤ 4 (4-entry cap exceeded)", len(u.Models))
	}
	// Verify the first 4 names — "E" must NOT appear.
	for _, m := range u.Models {
		if m.Name == "E" {
			t.Errorf("5th entry 'E' leaked through the 4-entry cap")
		}
	}
}

// TestFetch_PercentClamping verifies that negative and absurd (>1e12) percent
// values are clamped to 0, matching claudebar's guard.
// Also verifies that display_name is correctly mapped to ModelLimit.Name.
func TestFetch_PercentClamping(t *testing.T) {
	t.Parallel()

	const body = `{
		"five_hour": {"utilization": 0.1, "resets_at": "2026-07-07T15:00:00Z"},
		"seven_day": {"utilization": 0.1, "resets_at": "2026-07-14T00:00:00Z"},
		"limits": [
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "Negative"}}, "percent": -5, "resets_at": "2026-07-14T00:00:00Z"},
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "Huge"}}, "percent": 2e12, "resets_at": "2026-07-14T00:00:00Z"}
		]
	}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer ts.Close()

	ctx := context.Background()
	u, err := limits.Fetch(ctx, ts.Client(), "test-token", ts.URL)
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if len(u.Models) != 2 {
		t.Fatalf("Models len = %d, want 2", len(u.Models))
	}
	if u.Models[0].Name != "Negative" {
		t.Errorf("Models[0].Name = %q, want Negative", u.Models[0].Name)
	}
	if u.Models[0].Utilization != 0 {
		t.Errorf("Models[0].Utilization = %v, want 0 (negative percent clamped)", u.Models[0].Utilization)
	}
	if u.Models[1].Name != "Huge" {
		t.Errorf("Models[1].Name = %q, want Huge", u.Models[1].Name)
	}
	if u.Models[1].Utilization != 0 {
		t.Errorf("Models[1].Utilization = %v, want 0 (>1e12 percent clamped)", u.Models[1].Utilization)
	}
}

// TestFetch_EmptyDisplayNameFallback verifies that an empty or blank display_name
// falls back to "Model" (matching claudebar's clean/fallback logic).
func TestFetch_EmptyDisplayNameFallback(t *testing.T) {
	t.Parallel()

	const body = `{
		"five_hour": {"utilization": 0.1, "resets_at": "2026-07-07T15:00:00Z"},
		"seven_day": {"utilization": 0.1, "resets_at": "2026-07-14T00:00:00Z"},
		"limits": [
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": ""}}, "percent": 50, "resets_at": "2026-07-14T00:00:00Z"},
			{"kind": "weekly_scoped", "scope": {"model": {"display_name": "   "}}, "percent": 50, "resets_at": "2026-07-14T00:00:00Z"}
		]
	}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer ts.Close()

	ctx := context.Background()
	u, err := limits.Fetch(ctx, ts.Client(), "test-token", ts.URL)
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if len(u.Models) != 2 {
		t.Fatalf("Models len = %d, want 2", len(u.Models))
	}
	if u.Models[0].Name != "Model" {
		t.Errorf("Models[0].Name = %q, want Model (empty display_name fallback)", u.Models[0].Name)
	}
	if u.Models[1].Name != "Model" {
		t.Errorf("Models[1].Name = %q, want Model (blank display_name fallback)", u.Models[1].Name)
	}
}

// ----- Transport-error vs. rejection classification (Fix 1) -----

// TestCached_TransportError_WithStale_ReturnsStale verifies that a network-level
// failure during the token refresh (ErrRefreshTransient from oauth) is treated
// like a transient error: serve stale ≤7d rather than propagating the error.
func TestCached_TransportError_WithStale_ReturnsStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	if err := c.Write("usage.json", buildCachePayload(now.Add(-2*24*time.Hour))); err != nil {
		t.Fatal(err)
	}

	// Simulate a transport error (connection refused, timeout) from the token refresh.
	transientTok := func() (string, error) {
		return "", fmt.Errorf("%w: dial tcp: connection refused", oauth.ErrRefreshTransient)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() must not call usage API when tok() fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, transientTok,
		func() time.Time { return now }, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Cached() error = %v, want nil (transport error with stale → serve stale)", err)
	}
	if !u.Stale {
		t.Error("Usage.Stale = false, want true (stale data served on transport error)")
	}
	if u.FiveHour.Utilization != 0.42 {
		t.Errorf("FiveHour.Utilization = %v, want 0.42 (stale values)", u.FiveHour.Utilization)
	}
}

// TestCached_TransportError_NoCache_ReturnsLoading verifies that a network-level
// failure during token refresh with no usable stale data returns Loading... (zero
// Usage, nil error) — never propagates the error.
func TestCached_TransportError_NoCache_ReturnsLoading(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	// No cache file.

	now := time.Now()
	transientTok := func() (string, error) {
		return "", fmt.Errorf("%w: timeout", oauth.ErrRefreshTransient)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() must not call usage API when tok() fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	u, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, transientTok,
		func() time.Time { return now }, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Cached() error = %v, want nil (transport error, no cache → Loading...)", err)
	}
	if u.FiveHour.Utilization != 0 || u.SevenDay.Utilization != 0 {
		t.Errorf("Expected zero Usage (Loading...) on transport error + no cache, got %+v", u)
	}
}

// TestCached_ErrNoCredentials_PropagatesEvenWithStale verifies that ErrNoCredentials
// from tok() is ALWAYS propagated — never masked by stale cache. This signals to
// the UI that the user must run "claude" to log in.
func TestCached_ErrNoCredentials_PropagatesEvenWithStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	// Fresh-ish stale cache within 7-day window.
	if err := c.Write("usage.json", buildCachePayload(now.Add(-1*24*time.Hour))); err != nil {
		t.Fatal(err)
	}

	noCredsTok := func() (string, error) { return "", oauth.ErrNoCredentials }

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() must not call usage API when tok() fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	_, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, noCredsTok,
		func() time.Time { return now }, 50*time.Millisecond)
	if !errors.Is(err, oauth.ErrNoCredentials) {
		t.Errorf("Cached() error = %v, want oauth.ErrNoCredentials (must propagate even with stale cache)", err)
	}
}

// TestCached_ErrInvalidCredentials_PropagatesEvenWithStale verifies that
// ErrInvalidCredentials is always propagated — never masked by stale cache.
func TestCached_ErrInvalidCredentials_PropagatesEvenWithStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)

	now := time.Now()
	if err := c.Write("usage.json", buildCachePayload(now.Add(-1*24*time.Hour))); err != nil {
		t.Fatal(err)
	}

	invalidCredsTok := func() (string, error) { return "", oauth.ErrInvalidCredentials }

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() must not call usage API when tok() fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	_, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, invalidCredsTok,
		func() time.Time { return now }, 50*time.Millisecond)
	if !errors.Is(err, oauth.ErrInvalidCredentials) {
		t.Errorf("Cached() error = %v, want oauth.ErrInvalidCredentials (must propagate even with stale cache)", err)
	}
}

// TestCached_HTTP4xxRejection_ReturnsErrRefreshRejected verifies that an actual
// HTTP 4xx refresh response (ErrRefreshRejected) without usable cache propagates.
func TestCached_HTTP4xxRejection_NoCache_PropagatesErr(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c := cache.New(dir)
	// No cache.

	now := time.Now()
	rejectedTok := func() (string, error) {
		return "", fmt.Errorf("%w: status 401", oauth.ErrRefreshRejected)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Cached() must not call usage API when tok() fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	_, err := limits.Cached(context.Background(), c, ts.Client(), ts.URL, rejectedTok,
		func() time.Time { return now }, 50*time.Millisecond)
	if !errors.Is(err, oauth.ErrRefreshRejected) {
		t.Errorf("Cached() error = %v, want oauth.ErrRefreshRejected (HTTP rejection, no cache)", err)
	}
}

func containsString(haystack []byte, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 &&
		string(haystack) != "" &&
		(func() bool {
			for i := 0; i <= len(haystack)-len(needle); i++ {
				if string(haystack[i:i+len(needle)]) == needle {
					return true
				}
			}
			return false
		})()
}
