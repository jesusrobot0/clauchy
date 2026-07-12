// Package oauth handles Claude AI credential loading, token refresh, and
// lossless atomic write-back.
//
// Key design decisions:
//   - The credentials file nests tokens under "claudeAiOauth" and may carry
//     unrelated siblings (e.g., "mcpOAuth"). Both levels are decoded as
//     map[string]json.RawMessage so unknown fields are never dropped (ADR-10).
//   - expiresAt / expires_in decode via json.Number (int or float), truncated
//     to int64 milliseconds. A value > 1e12 is treated as an absolute epoch-ms
//     (not added to now) — a deliberate, documented divergence from claudebar.
//   - WriteBack creates the temp file in the same directory with mode 0600
//     BEFORE any token bytes, then renames (same-fs → atomic, 0600-first → no
//     token on a world-readable inode even for an instant).
//   - This package NEVER acquires any flock. The caller (limits.Cached) holds
//     .fetch.lock; acquiring it here would self-deadlock (Linux flock is not
//     re-entrant within a process).
package oauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Sentinel errors returned by this package.
var (
	// ErrNoCredentials is returned by Load when the credentials file is absent.
	ErrNoCredentials = errors.New("oauth: no credentials file")

	// ErrInvalidCredentials is returned by Load when the file exists but cannot
	// be parsed as a valid Claude credential document.
	ErrInvalidCredentials = errors.New("oauth: invalid credentials")

	// ErrRefreshRejected is returned when the token-refresh endpoint rejects the
	// grant with a 4xx response. Existing credentials are not modified.
	ErrRefreshRejected = errors.New("oauth: refresh rejected")

	// ErrRefreshTransient is returned when the HTTP transport fails or the token
	// endpoint responds with a retryable status (429 or 5xx).
	// Unlike ErrRefreshRejected, this is a transient condition and callers may
	// serve stale cached data rather than treating it as a credential error.
	ErrRefreshTransient = errors.New("oauth: refresh transient error")

	// ErrCredentialsChanged means another process replaced the shared Claude
	// credentials after they were loaded. Callers should adopt the newer file.
	ErrCredentialsChanged = errors.New("oauth: credentials changed concurrently")
)

// RefreshTimeout is the maximum time Token will wait for a token refresh
// response. Wire an http.Client with this timeout in the composition root.
const RefreshTimeout = 20 * time.Second

const maxRefreshResponseBytes = 1 << 20

// claudeClientID is the public OAuth client ID used by Claude Code.
// It is sent in the JSON body of the refresh request (never as a header).
const claudeClientID = "9d1c250a-e61b-48f6-aaeb-c55a64e3aecc"

// Config holds the injectable parameters required by Token.
// Production values are wired in main; tests pass httptest.Server URLs.
type Config struct {
	// CredentialsPath is the absolute path to the .credentials.json file.
	CredentialsPath string
	// TokenURL is the OAuth token refresh endpoint.
	// Production value: "https://platform.claude.com/v1/oauth/token"
	TokenURL string
}

// Credentials represents the parsed Claude credential document.
// The unexported rawDoc and rawInner fields retain the full original document
// (both levels) so WriteBack can re-encode it losslessly.
//
// SubscriptionType and RateLimitTier are optional string fields from the
// claudeAiOauth sub-tree. They are absent in some credential files; in that
// case both fields are the empty string. WriteBack preserves them verbatim via
// the retained rawInner map — no behavior change there.
type Credentials struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAtMs      int64
	SubscriptionType string // claudeAiOauth.subscriptionType, e.g. "max" or "pro"
	RateLimitTier    string // claudeAiOauth.rateLimitTier, e.g. "default_20x"

	rawDoc   map[string]json.RawMessage // entire top-level document
	rawInner map[string]json.RawMessage // claudeAiOauth sub-tree
	rawFile  []byte                     // exact source bytes for optimistic write-back
}

// PlanLabel derives a human-readable plan label from the OAuth credential
// fields subscriptionType and rateLimitTier.
//
// Derivation rules (mirroring claudebar):
//   - Empty sub → returns "".
//   - Capitalise the first letter of sub (e.g. "max" → "Max").
//   - If tier contains "5x" → append " 5x".
//   - If tier contains "20x" → append " 20x".
//   - Otherwise no suffix.
//
// Example: PlanLabel("max", "default_20x") → "Max 20x"
func PlanLabel(sub, tier string) string {
	if sub == "" {
		return ""
	}
	// Capitalise first letter; leave the rest as-is.
	runes := []rune(sub)
	runes[0] = unicode.ToUpper(runes[0])
	label := string(runes)

	switch {
	case strings.Contains(tier, "20x"):
		label += " 20x"
	case strings.Contains(tier, "5x"):
		label += " 5x"
	}
	return label
}

// Load reads and parses the credential file at path.
// Returns ErrNoCredentials if the file is absent, ErrInvalidCredentials if
// the JSON structure is unexpected or missing required fields.
func Load(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, ErrNoCredentials
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("oauth load %s: %w", path, err)
	}

	var rawDoc map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawDoc); err != nil {
		return Credentials{}, fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}

	innerRaw, ok := rawDoc["claudeAiOauth"]
	if !ok {
		return Credentials{}, fmt.Errorf("%w: missing claudeAiOauth key", ErrInvalidCredentials)
	}
	var rawInner map[string]json.RawMessage
	if err := json.Unmarshal(innerRaw, &rawInner); err != nil {
		return Credentials{}, fmt.Errorf("%w: claudeAiOauth: %v", ErrInvalidCredentials, err)
	}

	var accessToken string
	if v, ok := rawInner["accessToken"]; ok {
		if err := json.Unmarshal(v, &accessToken); err != nil {
			return Credentials{}, fmt.Errorf("%w: accessToken: %v", ErrInvalidCredentials, err)
		}
	}
	var refreshToken string
	if v, ok := rawInner["refreshToken"]; ok {
		if err := json.Unmarshal(v, &refreshToken); err != nil {
			return Credentials{}, fmt.Errorf("%w: refreshToken: %v", ErrInvalidCredentials, err)
		}
	}

	expiresAtMs, err := decodeExpiresAt(rawInner["expiresAt"])
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: expiresAt: %v", ErrInvalidCredentials, err)
	}

	// Optional plan fields — absent keys produce empty strings, not errors.
	var subscriptionType string
	if v, ok := rawInner["subscriptionType"]; ok {
		// Ignore unmarshal error; treat malformed value as absent.
		_ = json.Unmarshal(v, &subscriptionType)
	}
	var rateLimitTier string
	if v, ok := rawInner["rateLimitTier"]; ok {
		_ = json.Unmarshal(v, &rateLimitTier)
	}

	return Credentials{
		AccessToken:      accessToken,
		RefreshToken:     refreshToken,
		ExpiresAtMs:      expiresAtMs,
		SubscriptionType: subscriptionType,
		RateLimitTier:    rateLimitTier,
		rawDoc:           rawDoc,
		rawInner:         rawInner,
		rawFile:          append([]byte(nil), data...),
	}, nil
}

// NeedsRefresh reports whether the access token will expire within 5 minutes
// of now, meaning a refresh is required before the next API call.
func (c Credentials) NeedsRefresh(now time.Time) bool {
	// expiresAt / 1000 < now_unix + 300
	return c.ExpiresAtMs/1000 < now.Unix()+300
}

// WriteBack atomically writes updated token fields back to the credential file
// at path. It mutates only accessToken, refreshToken, and expiresAt in the
// retained raw document, preserving every other field.
//
// If newRefreshToken is empty, the existing refresh token is preserved — a
// refresh response that omits refresh_token must never blank it.
//
// The temp file is created in the same directory with mode 0600 before any
// token bytes are written, then renamed over path (atomic, same filesystem).
func (c Credentials) WriteBack(path, accessToken, refreshToken string, expiresAtMs int64) error {
	if refreshToken == "" {
		refreshToken = c.RefreshToken
	}

	inner := make(map[string]json.RawMessage, len(c.rawInner))
	for k, v := range c.rawInner {
		inner[k] = v
	}

	var err error
	if inner["accessToken"], err = json.Marshal(accessToken); err != nil {
		return fmt.Errorf("oauth writeback marshal accessToken: %w", err)
	}
	if inner["refreshToken"], err = json.Marshal(refreshToken); err != nil {
		return fmt.Errorf("oauth writeback marshal refreshToken: %w", err)
	}
	if inner["expiresAt"], err = json.Marshal(expiresAtMs); err != nil {
		return fmt.Errorf("oauth writeback marshal expiresAt: %w", err)
	}

	doc := make(map[string]json.RawMessage, len(c.rawDoc))
	for k, v := range c.rawDoc {
		doc[k] = v
	}
	doc["claudeAiOauth"], err = json.Marshal(inner)
	if err != nil {
		return fmt.Errorf("oauth writeback marshal claudeAiOauth: %w", err)
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("oauth writeback marshal doc: %w", err)
	}

	return atomicWrite0600(path, encoded, c.rawFile)
}

// Token returns a valid access token for the credentials at cfg.CredentialsPath.
// If the token is within 5 minutes of expiry, it is refreshed via a POST to
// cfg.TokenURL using h as the HTTP client.
//
// On a successful refresh, the credentials file is updated atomically (0600).
// On a 4xx/5xx refresh response, ErrRefreshRejected is returned and the
// credentials file is not modified.
//
// Token takes no *cache.Cache and acquires no flock. The caller (limits.Cached)
// already holds .fetch.lock; re-acquiring it would self-deadlock.
func Token(cfg Config, h *http.Client, now time.Time) (string, error) {
	creds, err := Load(cfg.CredentialsPath)
	if err != nil {
		return "", err // propagates ErrNoCredentials, ErrInvalidCredentials
	}

	if !creds.NeedsRefresh(now) {
		if strings.TrimSpace(creds.AccessToken) == "" {
			return "", fmt.Errorf("%w: accessToken is empty", ErrInvalidCredentials)
		}
		return creds.AccessToken, nil
	}

	return doRefresh(cfg, h, now, creds)
}

// Refresh forces a token refresh regardless of the access token expiry. It is
// used after the usage endpoint rejects an otherwise unexpired access token.
func Refresh(cfg Config, h *http.Client, now time.Time) (string, error) {
	creds, err := Load(cfg.CredentialsPath)
	if err != nil {
		return "", err
	}
	return doRefresh(cfg, h, now, creds)
}

// doRefresh performs the HTTP token refresh and updates the credentials file.
func doRefresh(cfg Config, h *http.Client, now time.Time, creds Credentials) (string, error) {
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return "", fmt.Errorf("%w: refreshToken is empty", ErrInvalidCredentials)
	}
	body := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     claudeClientID,
		"refresh_token": creds.RefreshToken,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("oauth refresh marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.TokenURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("oauth refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "github.com/jesusrobot0/clauchy/1.0")

	resp, err := h.Do(req)
	if err != nil {
		// Transport-layer failure (timeout, connection refused, DNS): transient,
		// not a credential rejection. Wrap as ErrRefreshTransient so callers
		// (limits.Cached) can serve stale data rather than treating it as a
		// permanent auth error.
		return "", fmt.Errorf("%w: %v", ErrRefreshTransient, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("oauth refresh read body: %w", err)
	}
	if len(respBytes) > maxRefreshResponseBytes {
		return "", fmt.Errorf("oauth refresh read body: response exceeds %d bytes", maxRefreshResponseBytes)
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: status %d", ErrRefreshTransient, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: status %d", ErrRefreshRejected, resp.StatusCode)
	}

	var refreshResp struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(respBytes, &refreshResp); err != nil {
		return "", fmt.Errorf("oauth refresh parse response: %w", err)
	}
	if strings.TrimSpace(refreshResp.AccessToken) == "" {
		return "", fmt.Errorf("oauth refresh parse response: access_token missing or empty")
	}

	expiresAtMs, err := resolveExpiresIn(refreshResp.ExpiresIn, now)
	if err != nil {
		return "", fmt.Errorf("oauth refresh expires_in: %w", err)
	}
	if expiresAtMs <= now.UnixMilli() {
		return "", fmt.Errorf("oauth refresh expires_in: expiry is not in the future")
	}

	// Re-read immediately before commit so unrelated fields changed by Claude
	// Code while the request was in flight are preserved. If another process
	// rotated the refresh token, its newer credential state wins.
	latest, err := Load(cfg.CredentialsPath)
	if err != nil {
		return "", fmt.Errorf("oauth refresh reload credentials: %w", err)
	}
	if latest.RefreshToken != creds.RefreshToken {
		if strings.TrimSpace(latest.AccessToken) == "" {
			return "", fmt.Errorf("%w: credentials changed during refresh", ErrRefreshTransient)
		}
		return latest.AccessToken, nil
	}

	if err := latest.WriteBack(cfg.CredentialsPath,
		refreshResp.AccessToken,
		refreshResp.RefreshToken, // empty → WriteBack preserves existing
		expiresAtMs,
	); errors.Is(err, ErrCredentialsChanged) {
		winner, loadErr := Load(cfg.CredentialsPath)
		if loadErr != nil || strings.TrimSpace(winner.AccessToken) == "" {
			return "", fmt.Errorf("%w: credentials changed during refresh", ErrRefreshTransient)
		}
		return winner.AccessToken, nil
	} else if err != nil {
		return "", fmt.Errorf("oauth refresh writeback: %w", err)
	}

	return refreshResp.AccessToken, nil
}

// resolveExpiresIn converts the expires_in field from the refresh response to
// an absolute epoch-millisecond timestamp.
//
// If the value is > 1e12 it is treated as an already-absolute epoch-ms value
// (NOT added to now). Otherwise it is relative seconds from now.
// This is a deliberate divergence from claudebar (which zeros values > 1e12).
func resolveExpiresIn(n json.Number, now time.Time) (int64, error) {
	if n == "" {
		return 0, fmt.Errorf("expires_in missing")
	}
	f, err := n.Float64()
	if err != nil {
		return 0, err
	}
	v := int64(f)
	if f > 1e12 {
		// Already an absolute epoch-ms value.
		return v, nil
	}
	// Relative seconds: convert to absolute ms.
	return now.UnixMilli() + v*1000, nil
}

// decodeExpiresAt parses the expiresAt field from the credential file.
// It accepts integer or float (e.g. 1.751e12) via json.Number, truncating
// to int64 milliseconds. A nil / missing field returns 0.
func decodeExpiresAt(raw json.RawMessage) (int64, error) {
	if raw == nil {
		return 0, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	f, err := n.Float64()
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

// atomicWrite0600 writes data to a temp file in the same directory as dest
// (mode 0600, created before any bytes), fsyncs, then renames over dest.
//
// The temp file is placed in the same directory so that os.Rename is
// guaranteed to be atomic (same filesystem, no cross-device move).
// Mode 0600 is set before any token bytes land on disk so that there is
// never a window where the inode is world-readable.
func atomicWrite0600(dest string, data, expected []byte) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("oauth mkdir: %w", err)
	}

	// os.CreateTemp uses os.O_CREATE|os.O_EXCL|0600 on Linux, satisfying the
	// "0600 before any token bytes" requirement without a separate chmod call.
	tmp, err := os.CreateTemp(dir, ".creds-tmp-*")
	if err != nil {
		return fmt.Errorf("oauth create temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("oauth write temp: %w", err)
	}

	if err := syscall.Fsync(int(tmp.Fd())); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("oauth fsync: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("oauth close temp: %w", err)
	}
	current, err := os.ReadFile(dest)
	if err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("oauth verify current credentials: %w", err)
	}
	if !bytes.Equal(current, expected) {
		os.Remove(tmpName)
		return ErrCredentialsChanged
	}

	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("oauth rename: %w", err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("oauth open directory for fsync: %w", err)
	}
	defer dirFile.Close()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("oauth fsync directory: %w", err)
	}
	return nil
}
