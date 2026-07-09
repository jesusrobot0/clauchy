// Package oauth_test exercises the oauth package in black-box style.
// Tests cover: credential loading, lossless round-trip, NeedsRefresh,
// token refresh via httptest.Server, and all sentinel error paths.
// No real home directory is used — all paths live in t.TempDir().
package oauth_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"clauchy/internal/oauth"
)

// ----- fixtures & helpers -----

// credentialDoc is the full credential fixture, including mcpOAuth and
// sibling fields inside claudeAiOauth — everything must survive a WriteBack.
const credentialDoc = `{
	"claudeAiOauth": {
		"accessToken":      "sk-ant-oat-old",
		"refreshToken":     "sk-ant-ort-old",
		"expiresAt":        1751900000000,
		"scopes":           ["user:inference", "user:profile"],
		"subscriptionType": "max",
		"rateLimitTier":    "default"
	},
	"mcpOAuth": {"someKey": "someValue", "nested": {"deep": true}}
}`

// buildDoc creates a minimal credential document with the given fields.
func buildDoc(t *testing.T, accessToken, refreshToken string, expiresAtMs int64) string {
	t.Helper()
	return fmt.Sprintf(`{
		"claudeAiOauth": {
			"accessToken":  %q,
			"refreshToken": %q,
			"expiresAt":    %d,
			"scopes":       ["user:inference"],
			"subscriptionType": "max",
			"rateLimitTier":    "default"
		},
		"mcpOAuth": {"key": "val"}
	}`, accessToken, refreshToken, expiresAtMs)
}

// writeCredentials writes content to <dir>/.credentials.json and returns the path.
func writeCredentials(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writeCredentials: %v", err)
	}
	return path
}

// refreshServer builds an httptest.Server that serves a valid refresh response.
// It records the received request in the supplied *http.Request pointer.
func refreshServer(t *testing.T, req **http.Request, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*req = r.Clone(r.Context())
		b, _ := io.ReadAll(r.Body)
		(*req).Body = io.NopCloser(bytesReader(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

type bytesReaderT struct {
	b   []byte
	pos int
}

func bytesReader(b []byte) *bytesReaderT { return &bytesReaderT{b: b} }
func (r *bytesReaderT) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}
func (r *bytesReaderT) Close() error { return nil }

// ----- Load -----

func TestLoad_ValidDoc(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeCredentials(t, dir, credentialDoc)

	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if creds.AccessToken != "sk-ant-oat-old" {
		t.Errorf("AccessToken = %q, want %q", creds.AccessToken, "sk-ant-oat-old")
	}
	if creds.RefreshToken != "sk-ant-ort-old" {
		t.Errorf("RefreshToken = %q, want %q", creds.RefreshToken, "sk-ant-ort-old")
	}
	if creds.ExpiresAtMs != 1751900000000 {
		t.Errorf("ExpiresAtMs = %d, want 1751900000000", creds.ExpiresAtMs)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := oauth.Load(filepath.Join(dir, ".credentials.json"))
	if !errors.Is(err, oauth.ErrNoCredentials) {
		t.Errorf("Load() error = %v, want ErrNoCredentials", err)
	}
}

func TestLoad_BadJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeCredentials(t, dir, `{not valid json`)
	_, err := oauth.Load(path)
	if !errors.Is(err, oauth.ErrInvalidCredentials) {
		t.Errorf("Load() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestLoad_ExpiresAtAsFloat(t *testing.T) {
	// 1.751e12 is a float representation; must parse as integer truncation.
	t.Parallel()
	dir := t.TempDir()
	doc := `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"ref","expiresAt":1.751e12}}`
	path := writeCredentials(t, dir, doc)
	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	// 1.751e12 = 1751000000000
	if creds.ExpiresAtMs != 1751000000000 {
		t.Errorf("ExpiresAtMs = %d, want 1751000000000 (float parsed as int)", creds.ExpiresAtMs)
	}
}

func TestLoad_ExpiresAtAbsoluteEpoch(t *testing.T) {
	// expiresAt > 1e12: must be kept as-is (treated as absolute epoch-ms, NOT added to now).
	t.Parallel()
	dir := t.TempDir()
	doc := `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"ref","expiresAt":1751900000000}}`
	path := writeCredentials(t, dir, doc)
	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if creds.ExpiresAtMs != 1751900000000 {
		t.Errorf("ExpiresAtMs = %d, want 1751900000000 (absolute epoch preserved)", creds.ExpiresAtMs)
	}
}

// ----- Lossless round-trip -----

func TestRoundTrip_LosslessMcpOAuthAndSiblings(t *testing.T) {
	// WriteBack must preserve mcpOAuth and sibling fields inside claudeAiOauth.
	t.Parallel()
	dir := t.TempDir()
	path := writeCredentials(t, dir, credentialDoc)

	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	const newExpiry = int64(1751900060000)
	if err := creds.WriteBack(path, "sk-ant-oat-new", "sk-ant-ort-old", newExpiry); err != nil {
		t.Fatalf("WriteBack() error: %v", err)
	}

	// Parse the written file raw to check structure.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("re-read JSON: %v", err)
	}

	// mcpOAuth must survive.
	if _, ok := top["mcpOAuth"]; !ok {
		t.Error("WriteBack() dropped top-level mcpOAuth key")
	}

	// claudeAiOauth siblings must survive.
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(top["claudeAiOauth"], &inner); err != nil {
		t.Fatalf("inner parse: %v", err)
	}
	for _, key := range []string{"scopes", "subscriptionType", "rateLimitTier"} {
		if _, ok := inner[key]; !ok {
			t.Errorf("WriteBack() dropped claudeAiOauth.%s", key)
		}
	}

	// New access token and expiry must be persisted.
	creds2, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() after WriteBack: %v", err)
	}
	if creds2.AccessToken != "sk-ant-oat-new" {
		t.Errorf("AccessToken after WriteBack = %q, want %q", creds2.AccessToken, "sk-ant-oat-new")
	}
	if creds2.ExpiresAtMs != newExpiry {
		t.Errorf("ExpiresAtMs after WriteBack = %d, want %d", creds2.ExpiresAtMs, newExpiry)
	}
}

func TestWriteBack_FileMode0600(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeCredentials(t, dir, credentialDoc)

	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if err := creds.WriteBack(path, "tok-new", "ref-new", 1751900060000); err != nil {
		t.Fatalf("WriteBack() error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file mode = 0%o, want 0600", info.Mode().Perm())
	}
}

// ----- NeedsRefresh -----

func TestNeedsRefresh(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0) // fixed reference

	cases := []struct {
		name        string
		expiresAtMs int64
		want        bool
	}{
		{"fresh 1h", now.UnixMilli() + 3600*1000, false},
		{"at threshold (exactly 300s left)", now.UnixMilli() + 300*1000, false},
		{"within threshold (299s left)", now.UnixMilli() + 299*1000, true},
		{"already expired", now.UnixMilli() - 1000, true},
		{"far future", now.UnixMilli() + 7200*1000, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := oauth.Credentials{ExpiresAtMs: tc.expiresAtMs}
			if got := c.NeedsRefresh(now); got != tc.want {
				t.Errorf("NeedsRefresh() = %v, want %v (expiresAtMs=%d, now=%d)",
					got, tc.want, tc.expiresAtMs, now.UnixMilli())
			}
		})
	}
}

// ----- Token — no refresh when fresh -----

func TestToken_FreshTokenNoHTTP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 3600*1000
	path := writeCredentials(t, dir, buildDoc(t, "sk-ant-oat-fresh", "sk-ant-ort", expiresAtMs))

	// httptest server that fails if called.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Token() made an HTTP request when token was still fresh")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}
	tok, err := oauth.Token(cfg, ts.Client(), now)
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}
	if tok != "sk-ant-oat-fresh" {
		t.Errorf("Token() = %q, want %q", tok, "sk-ant-oat-fresh")
	}
}

// ----- Token — refresh when near-expiry -----

func TestToken_RefreshRequestBody(t *testing.T) {
	// Verify the POST body contains grant_type, client_id (IN BODY), refresh_token.
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 100*1000 // < 300s → needs refresh

	var capturedReq *http.Request
	const refreshResp = `{"access_token":"sk-ant-oat-new","refresh_token":"sk-ant-ort-new","expires_in":3600}`
	ts := refreshServer(t, &capturedReq, http.StatusOK, refreshResp)
	defer ts.Close()

	path := writeCredentials(t, dir, buildDoc(t, "sk-ant-oat-old", "sk-ant-ort-old", expiresAtMs))
	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}

	tok, err := oauth.Token(cfg, ts.Client(), now)
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}
	if tok != "sk-ant-oat-new" {
		t.Errorf("Token() = %q, want %q", tok, "sk-ant-oat-new")
	}
	if capturedReq == nil {
		t.Fatal("no HTTP request captured")
	}

	// Verify method.
	if capturedReq.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", capturedReq.Method)
	}

	// Verify headers.
	if ct := capturedReq.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if ab := capturedReq.Header.Get("anthropic-beta"); ab != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", ab)
	}
	if ua := capturedReq.Header.Get("User-Agent"); ua == "" {
		t.Error("User-Agent header is missing")
	}

	// Verify body: grant_type, client_id (in body, not header), refresh_token.
	bodyBytes, err := io.ReadAll(capturedReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("body JSON: %v", err)
	}
	if body["grant_type"] != "refresh_token" {
		t.Errorf("body.grant_type = %q, want refresh_token", body["grant_type"])
	}
	if body["client_id"] == "" {
		t.Error("body.client_id is missing or empty (must be in body, not header)")
	}
	// Verify client_id is NOT a header.
	if capturedReq.Header.Get("X-Client-Id") != "" || capturedReq.Header.Get("Client-Id") != "" {
		t.Error("client_id must not appear as an HTTP header")
	}
	if body["refresh_token"] != "sk-ant-ort-old" {
		t.Errorf("body.refresh_token = %q, want sk-ant-ort-old", body["refresh_token"])
	}
}

func TestToken_RefreshUpdatesCredentialsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 100*1000 // near expiry

	const refreshResp = `{"access_token":"sk-ant-oat-new","refresh_token":"sk-ant-ort-new","expires_in":3600}`
	var captured *http.Request
	ts := refreshServer(t, &captured, http.StatusOK, refreshResp)
	defer ts.Close()

	path := writeCredentials(t, dir, buildDoc(t, "sk-ant-oat-old", "sk-ant-ort-old", expiresAtMs))
	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}
	if _, err := oauth.Token(cfg, ts.Client(), now); err != nil {
		t.Fatalf("Token() error: %v", err)
	}

	// Credentials file must carry the new access token.
	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() after refresh: %v", err)
	}
	if creds.AccessToken != "sk-ant-oat-new" {
		t.Errorf("AccessToken = %q, want sk-ant-oat-new", creds.AccessToken)
	}
	if creds.RefreshToken != "sk-ant-ort-new" {
		t.Errorf("RefreshToken = %q, want sk-ant-ort-new", creds.RefreshToken)
	}
	// expires_in=3600 (relative) → expiresAtMs = now.UnixMilli() + 3600*1000
	wantExpiry := now.UnixMilli() + 3600*1000
	if creds.ExpiresAtMs != wantExpiry {
		t.Errorf("ExpiresAtMs = %d, want %d (now + 3600s)", creds.ExpiresAtMs, wantExpiry)
	}
}

func TestToken_RefreshExpiresInAbsoluteEpoch(t *testing.T) {
	// expires_in > 1e12 → treat as absolute epoch-ms, NOT added to now.
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 100*1000

	const absEpoch = 1751900060000
	resp := fmt.Sprintf(`{"access_token":"tok","expires_in":%d}`, absEpoch)
	var captured *http.Request
	ts := refreshServer(t, &captured, http.StatusOK, resp)
	defer ts.Close()

	path := writeCredentials(t, dir, buildDoc(t, "old", "old-ref", expiresAtMs))
	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}
	if _, err := oauth.Token(cfg, ts.Client(), now); err != nil {
		t.Fatalf("Token() error: %v", err)
	}

	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() after refresh: %v", err)
	}
	// Must be the raw value, NOT now.UnixMilli() + absEpoch*1000.
	if creds.ExpiresAtMs != absEpoch {
		t.Errorf("ExpiresAtMs = %d, want %d (absolute epoch-ms, not added to now)",
			creds.ExpiresAtMs, absEpoch)
	}
}

func TestToken_RefreshPreservesExistingRefreshToken(t *testing.T) {
	// Response without refresh_token → existing token must be preserved (never blanked).
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 100*1000

	const refreshResp = `{"access_token":"tok-new","expires_in":3600}` // no refresh_token
	var captured *http.Request
	ts := refreshServer(t, &captured, http.StatusOK, refreshResp)
	defer ts.Close()

	path := writeCredentials(t, dir, buildDoc(t, "old-tok", "sk-ant-ort-preserved", expiresAtMs))
	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}
	if _, err := oauth.Token(cfg, ts.Client(), now); err != nil {
		t.Fatalf("Token() error: %v", err)
	}

	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() after refresh: %v", err)
	}
	if creds.RefreshToken != "sk-ant-ort-preserved" {
		t.Errorf("RefreshToken = %q, want sk-ant-ort-preserved (must not be blanked)", creds.RefreshToken)
	}
}

// ----- Token — refresh failure (4xx/5xx) -----

func TestToken_RefreshRejected_CredentialsUnchanged(t *testing.T) {
	// 4xx refresh → ErrRefreshRejected; credentials file must be byte-identical.
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 100*1000

	original := buildDoc(t, "sk-ant-oat-old", "sk-ant-ort-old", expiresAtMs)
	path := writeCredentials(t, dir, original)

	var captured *http.Request
	ts := refreshServer(t, &captured, http.StatusUnauthorized, `{"error":"invalid_grant"}`)
	defer ts.Close()

	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}
	_, err := oauth.Token(cfg, ts.Client(), now)
	if !errors.Is(err, oauth.ErrRefreshRejected) {
		t.Errorf("Token() error = %v, want ErrRefreshRejected", err)
	}

	// Credentials file must be byte-identical to the original.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Error("Token() wrote to credentials file on refresh failure (must be unchanged)")
	}
}

func TestToken_Refresh5xx(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 100*1000
	path := writeCredentials(t, dir, buildDoc(t, "old", "ref", expiresAtMs))

	var captured *http.Request
	ts := refreshServer(t, &captured, http.StatusInternalServerError, `{}`)
	defer ts.Close()

	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}
	_, err := oauth.Token(cfg, ts.Client(), now)
	if !errors.Is(err, oauth.ErrRefreshRejected) {
		t.Errorf("Token() error = %v, want ErrRefreshRejected", err)
	}
}

// ----- Load — SubscriptionType / RateLimitTier fields -----

func TestLoad_PlanFields(t *testing.T) {
	// SubscriptionType and RateLimitTier from claudeAiOauth must be exposed on Credentials.
	t.Parallel()
	dir := t.TempDir()
	path := writeCredentials(t, dir, credentialDoc) // has subscriptionType:"max", rateLimitTier:"default"

	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if creds.SubscriptionType != "max" {
		t.Errorf("SubscriptionType = %q, want %q", creds.SubscriptionType, "max")
	}
	if creds.RateLimitTier != "default" {
		t.Errorf("RateLimitTier = %q, want %q", creds.RateLimitTier, "default")
	}
}

func TestLoad_PlanFields_Absent(t *testing.T) {
	// When subscriptionType / rateLimitTier are absent, fields should be empty strings.
	t.Parallel()
	dir := t.TempDir()
	doc := `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"ref","expiresAt":1751900000000}}`
	path := writeCredentials(t, dir, doc)

	creds, err := oauth.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if creds.SubscriptionType != "" {
		t.Errorf("SubscriptionType = %q, want empty", creds.SubscriptionType)
	}
	if creds.RateLimitTier != "" {
		t.Errorf("RateLimitTier = %q, want empty", creds.RateLimitTier)
	}
}

// ----- PlanLabel helper -----

func TestPlanLabel(t *testing.T) {
	cases := []struct {
		sub  string
		tier string
		want string
	}{
		{"max", "default_20x", "Max 20x"},
		{"max", "default_5x", "Max 5x"},
		{"max", "", "Max"},
		{"pro", "", "Pro"},
		{"pro", "default_5x", "Pro 5x"},
		{"", "default_20x", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.sub+"_"+tc.tier, func(t *testing.T) {
			t.Parallel()
			got := oauth.PlanLabel(tc.sub, tc.tier)
			if got != tc.want {
				t.Errorf("PlanLabel(%q, %q) = %q, want %q", tc.sub, tc.tier, got, tc.want)
			}
		})
	}
}

// ─── Fix 6a: no token bytes in error strings ──────────────────────────────────

// TestLoad_NoTokenInErrors verifies that error strings returned by Load paths
// never contain raw credential token bytes. This guards against json parse
// errors or wrapping mistakes that could embed raw credential content.
func TestLoad_NoTokenInErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const fakeToken = "sk-ant-secret-should-not-appear-in-errors"

	// Credential doc where accessToken has a fake sentinel value but the JSON
	// is syntactically valid so Load parses successfully — no error from Load.
	// We then test paths that trigger errors to verify they don't include it.

	// Bad JSON at top level.
	badJSON := `{not valid json with token ` + fakeToken + `}`
	p1 := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(p1, []byte(badJSON), 0600); err != nil {
		t.Fatal(err)
	}
	_, err1 := oauth.Load(p1)
	if err1 == nil {
		t.Error("Load() on bad JSON should return error")
	} else if containsToken(err1.Error(), fakeToken) {
		t.Errorf("Load() error contains raw token %q in: %q", fakeToken, err1.Error())
	}

	// Missing claudeAiOauth key.
	p2 := filepath.Join(dir, "no_key.json")
	if err := os.WriteFile(p2, []byte(`{"other":"value"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err2 := oauth.Load(p2)
	if err2 != nil && containsToken(err2.Error(), fakeToken) {
		t.Errorf("Load() error (missing key) contains raw token in: %q", err2.Error())
	}
}

// TestToken_NoTokenInRefreshError verifies that error strings from a failed
// token refresh do not contain any raw access or refresh token bytes.
func TestToken_NoTokenInRefreshError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 100*1000 // near expiry → triggers refresh

	const fakeAccessToken = "sk-ant-oat-sentinel-value-abc123"
	const fakeRefreshToken = "sk-ant-ort-sentinel-value-xyz789"

	path := writeCredentials(t, dir, buildDoc(t, fakeAccessToken, fakeRefreshToken, expiresAtMs))

	// Server returns 401 → ErrRefreshRejected
	var captured *http.Request
	ts := refreshServer(t, &captured, http.StatusUnauthorized, `{"error":"invalid_grant"}`)
	defer ts.Close()

	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}
	_, err := oauth.Token(cfg, ts.Client(), now)
	if err == nil {
		t.Fatal("Token() should return error on 401 response")
	}

	errStr := err.Error()
	if containsToken(errStr, fakeAccessToken) {
		t.Errorf("Token() error contains raw access token %q in: %q", fakeAccessToken, errStr)
	}
	if containsToken(errStr, fakeRefreshToken) {
		t.Errorf("Token() error contains raw refresh token %q in: %q", fakeRefreshToken, errStr)
	}
}

// containsToken is a helper that returns true when target literally appears in s.
func containsToken(s, target string) bool {
	return target != "" && len(s) >= len(target) && func() bool {
		for i := 0; i <= len(s)-len(target); i++ {
			if s[i:i+len(target)] == target {
				return true
			}
		}
		return false
	}()
}

// ----- Token — no cache.Cache required -----

func TestToken_TakesNoCacheArg(t *testing.T) {
	// Compile-time check: oauth.Token signature must NOT accept *cache.Cache.
	// This is verified by the fact that this test calls Token with only
	// (Config, *http.Client, time.Time). If the signature changed, it wouldn't compile.
	t.Parallel()
	dir := t.TempDir()

	now := time.Unix(1700000000, 0)
	expiresAtMs := now.UnixMilli() + 3600*1000
	path := writeCredentials(t, dir, buildDoc(t, "tok", "ref", expiresAtMs))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	cfg := oauth.Config{CredentialsPath: path, TokenURL: ts.URL}
	// Three arguments only: Config, *http.Client, time.Time.
	tok, err := oauth.Token(cfg, ts.Client(), now)
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}
	if tok != "tok" {
		t.Errorf("Token() = %q, want tok", tok)
	}
}
