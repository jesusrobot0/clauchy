// Package status fetches the Claude status page (Atlassian Statuspage) summary,
// caches it in the cache directory (TTL 180s), and tags freshness via an
// embedded cachedAt timestamp inside the payload.
//
// Key design decisions:
//   - Freshness is derived from an EMBEDDED cachedAt field in status.json, NOT
//     file mtime (mirrors limits ADR-11).
//   - status.Cached is the SOLE owner of .status.lock (flock).
//   - ErrTransient wraps all non-200, transport, and parse errors so callers
//     can use errors.Is for classification.
//   - The stale ceiling is 24h (shorter than limits' 7d because status data is
//     externally owned and a >24h stale status page result is misleading).
//   - Worst maps the indicator + Claude Code component status to a single
//     severity string using worst-of-two semantics.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jesusrobot0/clauchy/internal/cache"
)

// ErrTransient is returned by Fetch (and propagated by Cached) when the
// network, server, or parse step fails transiently.
var ErrTransient = errors.New("status: transient error")

// FetchTimeout is the maximum time a single Fetch call may take.
const FetchTimeout = 2500 * time.Millisecond

// cacheTTL is the cache freshness window.
const cacheTTL = 180 * time.Second

// staleCeiling is the maximum age of stale data that Cached will serve.
const staleCeiling = 24 * time.Hour

// lockTimeout is the default bounded try-lock duration for .status.lock.
const lockTimeout = 3 * time.Second

// Status holds the parsed Statuspage summary, freshness-tagged by Cached.
type Status struct {
	// Indicator is the page-level severity: "none", "minor", "major", "critical".
	Indicator string
	// Description is the human-readable status line from the page object.
	Description string
	// ClaudeCode is the component status of the component whose Name is exactly
	// "Claude Code": "operational", "degraded_performance", "partial_outage",
	// "major_outage", or "" when the component is absent.
	ClaudeCode string
	// CachedAt is set by Cached when data is written to (or read from) the
	// cache. Fetch always returns a zero CachedAt.
	CachedAt time.Time
	// Stale is true when Cached returned data from a beyond-TTL cache because
	// a fresh fetch failed transiently.
	Stale bool
}

// ----- wire types -----

type wireStatus struct {
	Indicator   string `json:"indicator"`
	Description string `json:"description"`
}

type wireComponent struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// apiResponse models only the summary.json fields we consume; the page object
// (id, name, updated_at, …) is intentionally not mapped.
type apiResponse struct {
	Status     wireStatus      `json:"status"`
	Components []wireComponent `json:"components"`
}

// cachePayload is what we write to status.json. It embeds cachedAt for
// freshness determination (no file mtime, mirrors ADR-11 from limits).
type cachePayload struct {
	CachedAt    time.Time `json:"cachedAt"`
	Indicator   string    `json:"indicator"`
	Description string    `json:"description"`
	ClaudeCode  string    `json:"claudeCode"`
}

// ----- Fetch -----

// Fetch performs a single GET {baseURL}/api/v2/summary.json and returns the
// parsed Status. CachedAt and Stale are always zero/false from Fetch — they
// are only set by Cached.
//
// Returns a wrapped ErrTransient on non-200 HTTP status, transport error, or
// JSON parse failure. Unknown fields in the response are silently ignored.
func Fetch(ctx context.Context, h *http.Client, baseURL string) (Status, error) {
	url := baseURL + "/api/v2/summary.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Status{}, fmt.Errorf("%w: create request: %v", ErrTransient, err)
	}

	resp, err := h.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Status{}, fmt.Errorf("%w: status %d", ErrTransient, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Status{}, fmt.Errorf("%w: read body: %v", ErrTransient, err)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return Status{}, fmt.Errorf("%w: parse response: %v", ErrTransient, err)
	}

	return fromAPIResponse(apiResp), nil
}

// fromAPIResponse converts an API response into a Status.
func fromAPIResponse(r apiResponse) Status {
	s := Status{
		Indicator:   r.Status.Indicator,
		Description: r.Status.Description,
	}
	for _, comp := range r.Components {
		if comp.Name == "Claude Code" {
			s.ClaudeCode = comp.Status
			break
		}
	}
	return s
}

// ----- Cached -----

// Cached returns status data from the 180-second cache, fetching fresh data
// from baseURL when the cache is absent or stale.
//
// It is the SOLE owner of the .status.lock flock in the cache directory.
//
// now is an injectable clock seam for deterministic tests.
// lockTO overrides the default 3s lock timeout; pass 0 or omit to use the
// default. This seam keeps TestCached_LockTimeout_FallsBackToStale fast.
//
// Error semantics:
//   - On transient fetch failure with stale data within 24h: returns stale Status{Stale:true}, nil.
//   - On transient fetch failure with no usable stale data (>24h or absent):
//     returns (Status{}, wrapped ErrTransient).
//   - On lock contention past the deadline: falls back to stale; with no usable
//     stale data it returns (Status{}, wrapped ErrTransient) — the same no-data
//     contract as the transient-fetch path.
func Cached(
	ctx context.Context,
	c *cache.Cache,
	h *http.Client,
	baseURL string,
	now func() time.Time,
	lockTO ...time.Duration,
) (Status, error) {
	lt := lockTimeout
	if len(lockTO) > 0 && lockTO[0] > 0 {
		lt = lockTO[0]
	}

	currentNow := now()

	// 1. Quick path: read cache and check freshness.
	payload, cacheErr := readCachePayload(c)
	if cacheErr == nil {
		age := currentNow.Sub(payload.CachedAt)
		if age < cacheTTL {
			return fromCachePayload(payload, false), nil
		}
	}

	// 2. Determine if we have usable stale data (within 24h ceiling).
	var stalePayload *cachePayload
	if cacheErr == nil {
		age := currentNow.Sub(payload.CachedAt)
		if age <= staleCeiling {
			cp := payload
			stalePayload = &cp
		}
	}

	// 3. Acquire the exclusive .status.lock (bounded try-lock).
	var result Status
	var resultErr error

	lockErr := c.WithLock(".status.lock", lt, func() error {
		// 3a. Double-checked read inside the lock.
		payload2, cacheErr2 := readCachePayload(c)
		if cacheErr2 == nil {
			age2 := now().Sub(payload2.CachedAt)
			if age2 < cacheTTL {
				result = fromCachePayload(payload2, false)
				return nil // winner's data — skip fetch
			}
			// Update stale fallback from double-check (may be newer).
			if now().Sub(payload2.CachedAt) <= staleCeiling {
				cp2 := payload2
				stalePayload = &cp2
			}
		}

		// 3b. Fetch with bounded deadline.
		fCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
		defer cancel()

		s, fetchErr := Fetch(fCtx, h, baseURL)
		if fetchErr != nil {
			if stalePayload != nil {
				result = fromCachePayload(*stalePayload, true)
				return nil // serve stale on transient error
			}
			// No usable stale data — return error.
			resultErr = fetchErr
			return nil
		}

		// 3c. Write fresh payload with embedded cachedAt.
		n := now()
		cp := toCachePayload(s, n)
		if data, err := json.Marshal(cp); err == nil {
			_ = c.Write("status.json", data) // best-effort
		}

		s.CachedAt = n
		s.Stale = false
		result = s
		return nil
	})

	if errors.Is(lockErr, cache.ErrLockTimeout) {
		// Lock contention exceeded deadline → fall back to stale rather than hang.
		if stalePayload != nil {
			return fromCachePayload(*stalePayload, true), nil
		}
		// No usable stale data: same no-data contract as a transient fetch
		// failure, so callers classify both paths with errors.Is(ErrTransient).
		return Status{}, fmt.Errorf("%w: lock timeout with no usable cache: %v", ErrTransient, lockErr)
	}
	if lockErr != nil {
		return Status{}, lockErr
	}
	if resultErr != nil {
		return Status{}, resultErr
	}
	return result, nil
}

// ----- helpers -----

func readCachePayload(c *cache.Cache) (cachePayload, error) {
	raw, err := c.Read("status.json")
	if err != nil {
		return cachePayload{}, err
	}
	var p cachePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return cachePayload{}, fmt.Errorf("status: parse cache: %w", err)
	}
	return p, nil
}

func fromCachePayload(p cachePayload, stale bool) Status {
	return Status{
		Indicator:   p.Indicator,
		Description: p.Description,
		ClaudeCode:  p.ClaudeCode,
		CachedAt:    p.CachedAt,
		Stale:       stale,
	}
}

func toCachePayload(s Status, cachedAt time.Time) cachePayload {
	return cachePayload{
		CachedAt:    cachedAt,
		Indicator:   s.Indicator,
		Description: s.Description,
		ClaudeCode:  s.ClaudeCode,
	}
}

// ----- Worst -----

// indicatorRank maps page-level indicator strings to a severity rank.
// An EMPTY indicator maps to 0: it means "no data" (zero-value Status from an
// error fallback), which must never be reported as an incident. Non-empty
// unknown indicators map to 1 (minor) — non-operational but not alarming.
func indicatorRank(ind string) int {
	switch ind {
	case "none", "":
		return 0 // "" = absent data, not an unknown severity
	case "minor":
		return 1
	case "major":
		return 2
	case "critical":
		return 3
	default:
		return 1 // non-empty unknown → treat as minor
	}
}

// componentRank maps component status strings to a severity rank.
// under_maintenance falls into the default branch DELIBERATELY: planned
// maintenance still reduces availability (worth surfacing as minor) but is
// not an alarm-grade outage.
func componentRank(cs string) int {
	switch cs {
	case "operational", "":
		return 0
	case "degraded_performance":
		return 1
	case "partial_outage":
		return 2
	case "major_outage":
		return 3
	default:
		return 1 // unknown (incl. under_maintenance) → minor
	}
}

// rankToString converts a severity rank back to the canonical string.
func rankToString(rank int) string {
	switch rank {
	case 0:
		return "operational"
	case 1:
		return "minor"
	case 2:
		return "major"
	default:
		return "critical"
	}
}

// Worst returns the single worst severity derived from the page-level indicator
// and the Claude Code component status. The mapping is:
//
//	indicator:   none→0, minor→1, major→2, critical→3  (unknown→1)
//	component:   operational/""→0, degraded_performance→1, partial_outage→2, major_outage→3
//
// The higher rank wins. Output is one of:
//
//	"operational" | "minor" | "major" | "critical"
func Worst(s Status) string {
	r := indicatorRank(s.Indicator)
	cr := componentRank(s.ClaudeCode)
	if cr > r {
		r = cr
	}
	return rankToString(r)
}

// HumanLabel returns the human-readable incident label shared by the dashboard
// footer and the waybar tooltip:
//   - When the Claude Code component reports a non-operational state, the label
//     names it: "Claude Code: " + the machine status with underscores replaced
//     by spaces (partial_outage → "partial outage").
//   - Otherwise (component absent or operational) the incident is elsewhere on
//     the page, so the label is the page Description verbatim — prefixing it
//     with "Claude Code:" would misattribute the incident.
func HumanLabel(st Status) string {
	if st.ClaudeCode != "" && st.ClaudeCode != "operational" {
		return "Claude Code: " + strings.ReplaceAll(st.ClaudeCode, "_", " ")
	}
	return st.Description
}
