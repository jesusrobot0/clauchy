package transcript_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jesusrobot0/clauchy/internal/pricing"
	"github.com/jesusrobot0/clauchy/internal/transcript"
)

// fixedNow is a deterministic "now" used in all transcript tests.
// 2026-07-07 18:00:00 UTC — safely in the afternoon of July 7 UTC so that
// "today" is unambiguously July 7 in UTC and Mexico City (UTC-5 in July).
var fixedNow = time.Date(2026, 7, 7, 18, 0, 0, 0, time.UTC)

// mexicoCityLoc loads America/Mexico_City or skips the test if the timezone
// data is unavailable (some CI environments strip the tz database).
func mexicoCityLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		t.Skipf("timezone America/Mexico_City unavailable: %v", err)
	}
	return loc
}

// copyFixture copies a testdata/ fixture file into destDir under the same name.
func copyFixture(t *testing.T, destDir, fixture string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture %q: %v", fixture, err)
	}
	if err := os.WriteFile(filepath.Join(destDir, fixture), data, 0o644); err != nil {
		t.Fatalf("write fixture %q to temp dir: %v", fixture, err)
	}
}

// builtinTable returns the pricing table for test calls.
func builtinTable() pricing.Table { return pricing.Builtin() }

// ── Empty / constant-field tests ──────────────────────────────────────────────

func TestAggregate_EmptyRoots(t *testing.T) {
	t.Parallel()

	stats, err := transcript.Aggregate(nil, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate(nil): unexpected error: %v", err)
	}
	if stats.Streak != 0 {
		t.Errorf("Streak = %d, want 0", stats.Streak)
	}
	if stats.Today.Messages != 0 {
		t.Errorf("Today.Messages = %d, want 0", stats.Today.Messages)
	}
}

func TestAggregate_UsageKeyAllowsJSONWhitespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	line := `{"sessionId":"s1","type":"assistant","timestamp":"2026-07-07T12:00:00Z","message":{"id":"m1","type":"message","model":"claude-haiku-4-5","usage" : {"input_tokens":10,"output_tokens":5}}}`
	if err := os.WriteFile(filepath.Join(dir, "whitespace.jsonl"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Today.Messages != 1 || stats.Today.InputTokens != 10 {
		t.Fatalf("stats today = %+v, want one 10-token message", stats.Today)
	}
}

func TestAggregate_PricingDatePropagates(t *testing.T) {
	t.Parallel()

	stats, err := transcript.Aggregate(nil, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.PricingDate != pricing.PricingDate {
		t.Errorf("PricingDate = %q, want %q", stats.PricingDate, pricing.PricingDate)
	}
}

// ── Dedup tests ───────────────────────────────────────────────────────────────

// TestAggregate_DedupParentBeatsSubagent: same message.id in two files —
// isSidechain=false (parent) must win over isSidechain=true (subagent) regardless
// of token count.
func TestAggregate_DedupParentBeatsSubagent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// parent entry: 100 input, 50 output, isSidechain=false
	// sidechain entry: 999 input, 888 output, isSidechain=true — same message.id
	copyFixture(t, dir, "dedup_parent.jsonl")
	copyFixture(t, dir, "dedup_sidechain.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Today.Messages != 1 {
		t.Errorf("Today.Messages = %d, want 1 (dedup must yield one entry)", stats.Today.Messages)
	}
	if stats.Today.InputTokens != 100 {
		t.Errorf("Today.InputTokens = %d, want 100 (parent entry wins)", stats.Today.InputTokens)
	}
}

// TestAggregate_DedupTieKeepsHigherTokenCount: when both entries have the same
// isSidechain value, keep the one with the higher total token count.
func TestAggregate_DedupTieKeepsHigherTokenCount(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// dedup_tie.jsonl: two lines, same message.id, both isSidechain=true.
	// Line 1: 100 input + 50 output = 150 total.
	// Line 2: 200 input + 100 output = 300 total. ← winner
	copyFixture(t, dir, "dedup_tie.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Today.Messages != 1 {
		t.Errorf("Today.Messages = %d, want 1", stats.Today.Messages)
	}
	if stats.Today.InputTokens != 200 {
		t.Errorf("Today.InputTokens = %d, want 200 (higher-count entry wins on tie)", stats.Today.InputTokens)
	}
}

// ── Malformed lines ───────────────────────────────────────────────────────────

// TestAggregate_MalformedLines_ContinueOnError: a corrupt JSONL line must be
// silently skipped; the parser must not abort or return an error.
func TestAggregate_MalformedLines_ContinueOnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// malformed.jsonl: line 1 valid, line 2 corrupt JSON (has "usage:" so it
	// passes the byte-scan but fails json.Unmarshal), line 3 valid.
	copyFixture(t, dir, "malformed.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: unexpected error on malformed file: %v", err)
	}
	if stats.Today.Messages != 2 {
		t.Errorf("Today.Messages = %d, want 2 (corrupt line skipped, not aborted)", stats.Today.Messages)
	}
}

// ── Cache-creation split ──────────────────────────────────────────────────────

// TestAggregate_CacheCreation_Present: when the cache_creation sub-object is
// present, ephemeral_5m + ephemeral_1h are used; cache_creation_input_tokens is ignored.
func TestAggregate_CacheCreation_Present(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Entry: cache_creation_input_tokens=800 (should be ignored),
	//        cache_creation.ephemeral_5m=200, cache_creation.ephemeral_1h=300
	//        → CacheWriteTokens = 200 + 300 = 500
	copyFixture(t, dir, "cache_creation_present.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Today.CacheWriteTokens != 500 {
		t.Errorf("CacheWriteTokens = %d, want 500 (5m=200 + 1h=300)", stats.Today.CacheWriteTokens)
	}
}

// TestAggregate_CacheCreation_Absent: when cache_creation sub-object is absent,
// cache_creation_input_tokens falls back to the 5m bucket.
func TestAggregate_CacheCreation_Absent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Entry: no cache_creation sub-object, cache_creation_input_tokens=750
	//        → CacheWriteTokens = 750 (entire amount into 5m fallback)
	copyFixture(t, dir, "cache_creation_absent.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Today.CacheWriteTokens != 750 {
		t.Errorf("CacheWriteTokens = %d, want 750 (fallback to 5m bucket)", stats.Today.CacheWriteTokens)
	}
}

// ── Timezone boundary ─────────────────────────────────────────────────────────

// TestAggregate_TimezoneBoundary: UTC timestamps that cross midnight must be
// converted to the provided location before day assignment.
// With Mexico City (UTC-5 in July) and fixedNow = 2026-07-07T18:00Z:
//   - 2026-07-08T01:30Z → local 2026-07-07T20:30 → TODAY
//   - 2026-07-07T04:30Z → local 2026-07-06T23:30 → YESTERDAY
func TestAggregate_TimezoneBoundary(t *testing.T) {
	t.Parallel()

	loc := mexicoCityLoc(t)
	dir := t.TempDir()
	copyFixture(t, dir, "tz_boundary.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, loc)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	// 2026-07-08T01:30Z is still July 7 in Mexico City → Today should have 1 message.
	if stats.Today.Messages != 1 {
		t.Errorf("Today.Messages = %d, want 1 (2026-07-08T01:30Z is July 7 locally)", stats.Today.Messages)
	}
	// Both entries are within the 7-day window (July 1–7 local).
	if stats.Week.InputTokens == 0 {
		t.Error("Week.InputTokens = 0, want > 0 (both entries within 7-day window)")
	}
}

// ── Streak ────────────────────────────────────────────────────────────────────

// TestAggregate_Streak_TodayActive: consecutive days July 3–7 (all with messages)
// → streak = 5, counting from today.
func TestAggregate_Streak_TodayActive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyFixture(t, dir, "streak_five_consecutive.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Streak != 5 {
		t.Errorf("Streak = %d, want 5 (consecutive July 3–7)", stats.Streak)
	}
}

// TestAggregate_Streak_TodayGrace: entries exist for July 2–6 but not July 7
// (today). The streak is anchored at yesterday → streak = 5.
func TestAggregate_Streak_TodayGrace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyFixture(t, dir, "streak_grace.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Streak != 5 {
		t.Errorf("Streak = %d, want 5 (grace: today empty, count from yesterday July 6)", stats.Streak)
	}
}

// TestAggregate_Streak_Broken: entries on July 7 (today) and July 5 only;
// the gap on July 6 breaks the run → streak = 1.
func TestAggregate_Streak_Broken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyFixture(t, dir, "streak_broken.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Streak != 1 {
		t.Errorf("Streak = %d, want 1 (July 6 gap breaks streak)", stats.Streak)
	}
}

// ── Sessions + Messages ───────────────────────────────────────────────────────

// TestAggregate_TodaySessionsAndMessages: Today.Sessions counts distinct
// sessionIds; Today.Messages counts individual messages (after dedup).
func TestAggregate_TodaySessionsAndMessages(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// 3 messages from 2 sessions: sess_mst_1 (2 msgs) + sess_mst_2 (1 msg)
	copyFixture(t, dir, "multi_session_today.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Today.Messages != 3 {
		t.Errorf("Today.Messages = %d, want 3", stats.Today.Messages)
	}
	if stats.Today.Sessions != 2 {
		t.Errorf("Today.Sessions = %d, want 2", stats.Today.Sessions)
	}
}

// ── Unknown model ─────────────────────────────────────────────────────────────

// TestAggregate_UnknownModel: entries whose model ID is absent from the pricing
// table increment Stats.UnknownModels.
func TestAggregate_UnknownModel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyFixture(t, dir, "unknown_model.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.UnknownModels != 1 {
		t.Errorf("UnknownModels = %d, want 1 (claude-unknown-99 not in table)", stats.UnknownModels)
	}
}

// ── Week totals ───────────────────────────────────────────────────────────────

// TestAggregate_WeekTotals: entries across the 7-day window are all included
// in Week; entries outside the window are excluded.
func TestAggregate_WeekTotals(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// 5 entries on July 3–7 (all within the July 1–7 window).
	// Each has input=10, so Week.InputTokens should be 5×10 = 50.
	copyFixture(t, dir, "streak_five_consecutive.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if stats.Week.InputTokens != 50 {
		t.Errorf("Week.InputTokens = %d, want 50 (5 entries × 10 input)", stats.Week.InputTokens)
	}
}

// ── Synthetic entry exclusion ─────────────────────────────────────────────────

// TestAggregate_SyntheticExcluded: entries whose model is "<synthetic>" are
// Claude Code local error placeholders and must be silently excluded from all
// aggregation — Models7d, totals, Messages, Streak, and UnknownModels.
//
// Fixture layout (synthetic_mixed.jsonl):
//   - msg_syn_001: model="claude-opus-4-8", today → real entry (counted)
//   - msg_syn_002: model="<synthetic>", today → must be excluded
//   - msg_syn_003: model="claude-sonnet-4-6", today → real entry (counted)
func TestAggregate_SyntheticExcluded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyFixture(t, dir, "synthetic_mixed.jsonl")

	stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

	// Only the two real entries must appear.
	if stats.Today.Messages != 2 {
		t.Errorf("Today.Messages = %d, want 2 (<synthetic> must be excluded)", stats.Today.Messages)
	}
	// The synthetic entry carried input=500; if it leaked, total would be > 200.
	if stats.Today.InputTokens != 200 {
		t.Errorf("Today.InputTokens = %d, want 200 (100+100; <synthetic>=500 excluded)", stats.Today.InputTokens)
	}
	// Streak must not count the synthetic day if it were the only entry (here
	// real entries exist so streak = 1; the synthetic must not inflate it).
	if stats.Streak != 1 {
		t.Errorf("Streak = %d, want 1", stats.Streak)
	}
	// <synthetic> must NOT appear in Models7d.
	for _, m := range stats.Models7d {
		if m.Model == "<synthetic>" {
			t.Errorf("Models7d contains <synthetic> entry — must be excluded")
		}
	}
	// <synthetic> must NOT count as an unknown model.
	if stats.UnknownModels != 0 {
		t.Errorf("UnknownModels = %d, want 0 (<synthetic> is not a real model)", stats.UnknownModels)
	}
}

// ── Models7d deterministic ordering ──────────────────────────────────────────

// TestAggregate_Models7dDeterministicOrder: Models7d must be sorted by total
// tokens (input+output) descending; ties broken by model name ascending.
// Because Go map iteration is randomised, the sort must be explicit in the
// implementation — this test calls Aggregate multiple times and asserts the
// order is identical every time.
//
// Fixture layout (tied_tokens.jsonl):
//   - model="claude-opus-4-8"    input=200 output=100 → total=300
//   - model="claude-sonnet-4-6"  input=200 output=100 → total=300 (tie)
//   - model="claude-haiku-4-5"   input=100 output=50  → total=150
//
// Expected order: [opus=300, sonnet=300, haiku=150] — opus before sonnet
// because "claude-opus-4-8" < "claude-sonnet-4-6" lexicographically on tie.
func TestAggregate_Models7dDeterministicOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyFixture(t, dir, "tied_tokens.jsonl")

	// Run Aggregate 10 times; every run must return the same Models7d order.
	const runs = 10
	var reference []string
	for i := 0; i < runs; i++ {
		stats, err := transcript.Aggregate([]string{dir}, builtinTable(), fixedNow, time.UTC)
		if err != nil {
			t.Fatalf("run %d: Aggregate: %v", i, err)
		}
		if len(stats.Models7d) != 3 {
			t.Fatalf("run %d: len(Models7d) = %d, want 3", i, len(stats.Models7d))
		}

		order := make([]string, len(stats.Models7d))
		for j, m := range stats.Models7d {
			order[j] = m.Model
		}

		if i == 0 {
			reference = order
			// Verify the first run has the expected ordering.
			wantOrder := []string{"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5"}
			for k, want := range wantOrder {
				if order[k] != want {
					t.Errorf("run 0: Models7d[%d].Model = %q, want %q", k, order[k], want)
				}
			}
			continue
		}

		for k, got := range order {
			if got != reference[k] {
				t.Errorf("run %d: Models7d[%d].Model = %q, want %q (non-deterministic order)", i, k, got, reference[k])
			}
		}
	}
}
