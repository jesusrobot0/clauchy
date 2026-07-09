// Package limits fetches Claude API usage from the /api/oauth/usage endpoint,
// caches it in the cache directory (TTL 90s), and tags freshness via an
// embedded cachedAt timestamp inside the payload.
//
// Key design decisions:
//   - Freshness is derived from an EMBEDDED cachedAt field in usage.json, NOT
//     file mtime (ADR-11). This makes the TTL portable and immune to backup/
//     restore or editor rewrites.
//   - limits.Cached is the SOLE owner of .fetch.lock (flock). oauth.Token
//     runs inside that lock and must NOT acquire any lock of its own.
//   - limits does NOT import oauth directly. The TokenFunc seam (ADR-3) keeps
//     the packages decoupled: main binds oauth.Token into a TokenFunc closure.
//     However, limits imports oauth's sentinel errors for classification.
//   - ErrNoCredentials / ErrInvalidCredentials from tok() always propagate
//     immediately — they signal a broken credential state and must never be
//     masked by stale cache ("Run claude to log in" must surface).
//   - ErrRefreshTransient from tok() (transport failure during token refresh)
//     is treated like a fetch error: serve stale ≤7d or return Loading...
//   - ErrRefreshRejected from tok() with usable stale data → serve stale.
//     Without stale data → propagate (persistent auth rejection).
//   - Fetch (HTTP/network) errors are always suppressed to nil (Loading...) or
//     stale-fallback — never shown as raw errors in the bar.
package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/jesusrobot0/clauchy/internal/cache"
	"github.com/jesusrobot0/clauchy/internal/oauth"
)

// Sentinel errors.
var (
	// ErrTransient is returned by Fetch when the network or server is
	// temporarily unavailable (HTTP 5xx, context deadline, network error).
	ErrTransient = errors.New("limits: transient error")
)

// FetchTimeout is the maximum time a single Fetch call may take.
// Cached wraps the context with this deadline before calling Fetch.
const FetchTimeout = 2500 * time.Millisecond

// staleCeiling is the maximum age of stale data that can be served.
const staleCeiling = 7 * 24 * time.Hour

// cacheTTL is the fresh cache TTL.
const cacheTTL = 90 * time.Second

// lockTimeout is the default bounded try-lock duration for .fetch.lock.
// Tests that need a fast timeout use Cached's lockTimeout parameter via
// the CachedOpts helper; production code always passes DefaultLockTimeout.
const lockTimeout = 3 * time.Second

// DefaultLockTimeout is the production lock timeout exported for tests.
const DefaultLockTimeout = lockTimeout

// TokenFunc is the injectable token provider. main binds oauth.Token into
// a closure of this type, keeping limits decoupled from oauth.
type TokenFunc func() (string, error)

// Window represents a usage window returned by the API.
type Window struct {
	Utilization float64
	ResetsAt    time.Time
}

// ModelLimit is per-model usage data from the limits[] array.
type ModelLimit struct {
	Name        string
	Utilization float64
	ResetsAt    time.Time
}

// Extra holds the raw extra_usage JSON blob from the API, when present.
// It preserves the bytes verbatim for future use without making assumptions
// about the schema.
type Extra json.RawMessage

// UnmarshalJSON satisfies json.Unmarshaler so *Extra works with json.Unmarshal.
func (e *Extra) UnmarshalJSON(b []byte) error {
	*e = Extra(append([]byte(nil), b...))
	return nil
}

// MarshalJSON satisfies json.Marshaler so *Extra round-trips through JSON.
func (e Extra) MarshalJSON() ([]byte, error) {
	if e == nil {
		return []byte("null"), nil
	}
	return json.RawMessage(e), nil
}

// Usage is the fully parsed (and freshness-tagged) usage data.
// CachedAt and Stale are set by Cached(), not returned by the API.
type Usage struct {
	FiveHour       Window
	SevenDay       Window
	SevenDaySonnet *Window
	Models         []ModelLimit
	Extra          *Extra
	CachedAt       time.Time
	Stale          bool
}

// ----- wire types (internal JSON shapes) -----

type wireWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// wireModelLimit is the NORMALIZED form used in the cache payload (usage.json).
// This maintains backward-compatibility: old cache files written before the
// real-schema fix also use name/utilization/resets_at and parse without error.
type wireModelLimit struct {
	Name        string  `json:"name"`
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// wireAPIScopeModel is the model sub-object inside a weekly_scoped scope.
type wireAPIScopeModel struct {
	DisplayName string `json:"display_name"`
}

// wireAPIScope is the scope object in a limits[] entry.
type wireAPIScope struct {
	Model *wireAPIScopeModel `json:"model,omitempty"`
}

// wireAPIModelLimit is the REAL API wire shape for limits[] entries.
// Only entries with Kind=="weekly_scoped" AND a non-nil Scope.Model are used.
// Percent is a 0-100 integer percent value (not a fraction); negatives and
// values >1e12 are clamped to 0 (claudebar guard).
type wireAPIModelLimit struct {
	Kind     string       `json:"kind"`
	Scope    wireAPIScope `json:"scope"`
	Percent  float64      `json:"percent"`
	ResetsAt string       `json:"resets_at"`
}

// apiResponse is the JSON shape returned by GET /api/oauth/usage.
// Limits uses the REAL API schema (kind/scope.model/percent), not the legacy
// name/utilization shape our old implementation incorrectly assumed.
type apiResponse struct {
	FiveHour       wireWindow          `json:"five_hour"`
	SevenDay       wireWindow          `json:"seven_day"`
	SevenDaySonnet *wireWindow         `json:"seven_day_sonnet,omitempty"`
	Limits         []wireAPIModelLimit `json:"limits,omitempty"`
	ExtraUsage     *Extra              `json:"extra_usage,omitempty"`
}

// cachePayload is what we write to usage.json. It is the API response plus
// an embedded cachedAt timestamp (ADR-11 — freshness from payload, not mtime).
// Limits are stored in normalized form (display_name resolved, percent stored
// as Utilization, Sonnet-dedup and 4-entry cap already applied) so old cache
// payloads (name/utilization/resets_at) continue to parse without crashing.
type cachePayload struct {
	CachedAt       time.Time        `json:"cachedAt"`
	FiveHour       wireWindow       `json:"five_hour"`
	SevenDay       wireWindow       `json:"seven_day"`
	SevenDaySonnet *wireWindow      `json:"seven_day_sonnet,omitempty"`
	Limits         []wireModelLimit `json:"limits,omitempty"` // normalized cache form
	ExtraUsage     *Extra           `json:"extra_usage,omitempty"`
}

// ----- Fetch -----

// Fetch performs a single GET /api/oauth/usage request and returns the parsed
// Usage. The ctx should already have a FetchTimeout deadline applied by Cached.
//
// Returns ErrTransient on HTTP 5xx, network errors, and context deadline.
// CachedAt and Stale are NOT set by Fetch; those are set by Cached.
func Fetch(ctx context.Context, h *http.Client, token, baseURL string) (Usage, error) {
	url := baseURL + "/api/oauth/usage"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Usage{}, fmt.Errorf("%w: create request: %v", ErrTransient, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := h.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Usage{}, fmt.Errorf("%w: status %d", ErrTransient, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Usage{}, fmt.Errorf("%w: read body: %v", ErrTransient, err)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return Usage{}, fmt.Errorf("%w: parse response: %v", ErrTransient, err)
	}

	return toUsageFromAPI(apiResp)
}

// ----- Cached -----

// Cached returns usage data from the 90-second in-process cache, fetching
// fresh data from baseURL when the cache is absent or stale.
//
// It is the SOLE owner of the .fetch.lock flock in the cache directory.
// The tok function provides a valid Bearer token (may trigger an oauth
// refresh; that refresh runs inside the held lock).
//
// now is an injectable clock seam for deterministic tests.
// lockTO overrides the default 3s lock timeout; pass 0 to use the default.
// This seam keeps TestCached_LockTimeout_FallsBackToStale fast.
//
// Error semantics:
//   - oauth.ErrNoCredentials / oauth.ErrInvalidCredentials from tok(): always
//     propagated immediately — even when stale data is present. These signal a
//     broken credential state ("Run claude to log in") and must not be masked.
//   - oauth.ErrRefreshTransient from tok() (transport failure during token
//     refresh): treated as a fetch error — serve stale ≤7d, else Loading...
//   - oauth.ErrRefreshRejected from tok() with stale ≤7d: serve stale.
//     Without usable stale data: propagate.
//   - Fetch (HTTP/network) errors: suppressed — returns (Usage{}, nil) when no
//     stale fallback, which signals "Loading..." to the rendering layer.
func Cached(
	ctx context.Context,
	c *cache.Cache,
	h *http.Client,
	baseURL string,
	tok TokenFunc,
	now func() time.Time,
	lockTO ...time.Duration,
) (Usage, error) {
	lt := lockTimeout
	if len(lockTO) > 0 && lockTO[0] > 0 {
		lt = lockTO[0]
	}

	currentNow := now()

	// 1. Quick path: read cache and check if it's still fresh.
	payload, cacheErr := readCachePayload(c)
	if cacheErr == nil {
		age := currentNow.Sub(payload.CachedAt)
		if age < cacheTTL {
			return toUsageFromCache(payload, false), nil
		}
	}

	// 2. Determine if we have usable stale data (≤7d ceiling).
	var stalePayload *cachePayload
	if cacheErr == nil {
		age := currentNow.Sub(payload.CachedAt)
		if age <= staleCeiling {
			cp := payload // copy
			stalePayload = &cp
		}
	}

	// 3. Acquire the exclusive .fetch.lock (bounded try-lock).
	var result Usage

	lockErr := c.WithLock(".fetch.lock", lt, func() error {
		// 3a. Double-check: re-read cache inside the lock.
		// A competitor goroutine may have refreshed while we were waiting.
		payload2, cacheErr2 := readCachePayload(c)
		if cacheErr2 == nil {
			age2 := now().Sub(payload2.CachedAt)
			if age2 < cacheTTL {
				result = toUsageFromCache(payload2, false)
				return nil // winner's data — skip fetch
			}
			// Update stale fallback from double-check (may be newer).
			age2abs := now().Sub(payload2.CachedAt)
			if age2abs <= staleCeiling {
				cp2 := payload2
				stalePayload = &cp2
			}
		}

		// 3b. Get access token.
		tokStr, tokErr := tok()
		if tokErr != nil {
			// ErrNoCredentials and ErrInvalidCredentials must ALWAYS propagate
			// immediately — never masked by stale data. These mean "no/broken
			// credentials" and the UI must show the re-auth prompt.
			if errors.Is(tokErr, oauth.ErrNoCredentials) || errors.Is(tokErr, oauth.ErrInvalidCredentials) {
				return tokErr
			}
			// ErrRefreshTransient: transport failure during token refresh.
			// Behaves like a fetch error: serve stale if available, else Loading...
			if errors.Is(tokErr, oauth.ErrRefreshTransient) {
				if stalePayload != nil {
					result = toUsageFromCache(*stalePayload, true)
					return nil
				}
				result = Usage{}
				return nil
			}
			// ErrRefreshRejected (HTTP 4xx/5xx from auth server): serve stale
			// if available; propagate only when there is no usable stale data.
			if stalePayload != nil {
				result = toUsageFromCache(*stalePayload, true)
				return nil
			}
			return tokErr
		}

		// 3c. Fetch with bounded deadline.
		fCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
		defer cancel()

		u, fetchErr := Fetch(fCtx, h, tokStr, baseURL)
		if fetchErr != nil {
			if stalePayload != nil {
				result = toUsageFromCache(*stalePayload, true)
				return nil // serve stale on transient error
			}
			// No usable stale data — return zero Usage (Loading... state).
			// Do NOT propagate fetch errors; they are always transient.
			result = Usage{}
			return nil
		}

		// 3d. Write fresh payload with embedded cachedAt.
		n := now()
		cp := toCachePayload(u, n)
		if data, err := json.Marshal(cp); err == nil {
			_ = c.Write("usage.json", data) // best-effort; don't fail the caller
		}

		u.CachedAt = n
		u.Stale = false
		result = u
		return nil
	})

	if errors.Is(lockErr, cache.ErrLockTimeout) {
		// Lock contention exceeded deadline → fall back to stale rather than hang.
		if stalePayload != nil {
			return toUsageFromCache(*stalePayload, true), nil
		}
		return Usage{}, nil // Loading...
	}
	if lockErr != nil {
		// Non-ErrLockTimeout: one of the tok errors we deliberately propagated.
		return Usage{}, lockErr
	}
	return result, nil
}

// ----- conversion helpers -----

func readCachePayload(c *cache.Cache) (cachePayload, error) {
	raw, err := c.Read("usage.json")
	if err != nil {
		return cachePayload{}, err
	}
	var p cachePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return cachePayload{}, fmt.Errorf("limits: parse cache: %w", err)
	}
	return p, nil
}

func toUsageFromAPI(r apiResponse) (Usage, error) {
	fh, err := parseWindow(r.FiveHour)
	if err != nil {
		return Usage{}, fmt.Errorf("%w: five_hour: %v", ErrTransient, err)
	}
	sd, err := parseWindow(r.SevenDay)
	if err != nil {
		return Usage{}, fmt.Errorf("%w: seven_day: %v", ErrTransient, err)
	}

	u := Usage{
		FiveHour: fh,
		SevenDay: sd,
		Extra:    r.ExtraUsage,
	}

	if r.SevenDaySonnet != nil {
		w, err := parseWindow(*r.SevenDaySonnet)
		if err != nil {
			return Usage{}, fmt.Errorf("%w: seven_day_sonnet: %v", ErrTransient, err)
		}
		u.SevenDaySonnet = &w
	}

	// Parse limits[] using the REAL API schema:
	//   - kind must be "weekly_scoped"
	//   - scope.model must be a well-formed object
	//   - Name ← scope.model.display_name (control-stripped; fallback "Model" if blank)
	//   - Utilization ← percent (clamped: negative or >1e12 → 0, matching claudebar)
	//   - Skip a Sonnet entry when seven_day_sonnet is already present (dedup)
	//   - Cap at 4 entries (hostile payloads must not grow the tooltip)
	hasSonnet := r.SevenDaySonnet != nil
	for _, ml := range r.Limits {
		if len(u.Models) >= 4 {
			break // 4-entry cap
		}
		if ml.Kind != "weekly_scoped" {
			continue
		}
		if ml.Scope.Model == nil {
			continue
		}
		name := cleanDisplayName(ml.Scope.Model.DisplayName)
		if hasSonnet && name == "Sonnet" {
			continue // dedup: seven_day_sonnet already covers this window
		}
		pctVal := clampPercent(ml.Percent)
		resetsAt, err := time.Parse(time.RFC3339, ml.ResetsAt)
		if err != nil {
			continue // skip entries with unparseable resets_at
		}
		u.Models = append(u.Models, ModelLimit{
			Name:        name,
			Utilization: pctVal,
			ResetsAt:    resetsAt,
		})
	}

	return u, nil
}

// toUsageFromCache restores a Usage from the cache payload. It parses the
// normalized model limits (name/utilization/resets_at) stored in the cache
// directly, without routing through toUsageFromAPI, so the cache and API wire
// formats are fully independent.
func toUsageFromCache(p cachePayload, stale bool) Usage {
	fh, err := parseWindow(p.FiveHour)
	if err != nil {
		// Corrupt cache entry — return a minimal safe value.
		return Usage{CachedAt: p.CachedAt, Stale: stale}
	}
	sd, err := parseWindow(p.SevenDay)
	if err != nil {
		return Usage{CachedAt: p.CachedAt, Stale: stale}
	}

	u := Usage{
		FiveHour: fh,
		SevenDay: sd,
		Extra:    p.ExtraUsage,
		CachedAt: p.CachedAt,
		Stale:    stale,
	}

	if p.SevenDaySonnet != nil {
		w, err := parseWindow(*p.SevenDaySonnet)
		if err == nil {
			u.SevenDaySonnet = &w
		}
	}

	// Restore normalized model limits from cache (name/utilization already resolved).
	for _, ml := range p.Limits {
		resetsAt, err := time.Parse(time.RFC3339, ml.ResetsAt)
		if err != nil {
			continue // skip corrupt cache entries
		}
		u.Models = append(u.Models, ModelLimit{
			Name:        ml.Name,
			Utilization: ml.Utilization,
			ResetsAt:    resetsAt,
		})
	}

	return u
}

// cleanDisplayName strips control characters from a display name and returns
// "Model" when the result is blank. Mirrors claudebar's jq clean function:
//
//	def clean: gsub("[[:cntrl:]]"; " ") | if gsub(" "; "") == "" then "Model" else . end;
func cleanDisplayName(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if strings.ReplaceAll(cleaned, " ", "") == "" {
		return "Model"
	}
	return cleaned
}

// clampPercent returns 0 when f is negative or exceeds 1e12, mirroring
// claudebar's guard: if (. > 1e12 or . < 0) then 0 else . end.
func clampPercent(f float64) float64 {
	if f < 0 || f > 1e12 {
		return 0
	}
	return f
}

func toCachePayload(u Usage, cachedAt time.Time) cachePayload {
	cp := cachePayload{
		CachedAt: cachedAt,
		FiveHour: wireWindow{
			Utilization: u.FiveHour.Utilization,
			ResetsAt:    u.FiveHour.ResetsAt.UTC().Format(time.RFC3339),
		},
		SevenDay: wireWindow{
			Utilization: u.SevenDay.Utilization,
			ResetsAt:    u.SevenDay.ResetsAt.UTC().Format(time.RFC3339),
		},
		ExtraUsage: u.Extra,
	}
	if u.SevenDaySonnet != nil {
		w := wireWindow{
			Utilization: u.SevenDaySonnet.Utilization,
			ResetsAt:    u.SevenDaySonnet.ResetsAt.UTC().Format(time.RFC3339),
		}
		cp.SevenDaySonnet = &w
	}
	for _, ml := range u.Models {
		cp.Limits = append(cp.Limits, wireModelLimit{
			Name:        ml.Name,
			Utilization: ml.Utilization,
			ResetsAt:    ml.ResetsAt.UTC().Format(time.RFC3339),
		})
	}
	return cp
}

func parseWindow(w wireWindow) (Window, error) {
	resetsAt, err := time.Parse(time.RFC3339, w.ResetsAt)
	if err != nil {
		return Window{}, fmt.Errorf("parse resets_at %q: %w", w.ResetsAt, err)
	}
	return Window{
		Utilization: w.Utilization,
		ResetsAt:    resetsAt,
	}, nil
}
