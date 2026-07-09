// Package transcript — white-box tests that need access to unexported types and
// functions. These tests are in package transcript (not transcript_test) so they
// can call parseFileWithMaxLine and inspect resolvedEntry directly.
package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── Fix 4: oversized-line drain (bufio.Reader replaces bufio.Scanner) ────────

// TestParseFile_OversizedLine_ContinuesAfterDrain verifies that a line whose
// length exceeds the configured buffer size is SKIPPED (drained) without
// stopping the rest of the file. The entry after the oversized line must be
// parsed and counted normally.
//
// The test uses a small injected buffer size (256 bytes) so the fixture can be
// kept small and the test runs in milliseconds.
func TestParseFile_OversizedLine_ContinuesAfterDrain(t *testing.T) {
	// Use 1024 bytes as the small buffer; the valid lines are ~300 bytes each
	// (well within 1024), and line2 is padded to >1024 to trigger the drain path.
	const smallBuf = 1024

	// Build fixture in a temp file:
	//   Line 1: valid assistant entry with usage (fits in smallBuf)
	//   Line 2: a line > smallBuf bytes with "usage": to pass phase-1, but
	//            actually malformed JSON — just a very long run of chars.
	//            This exercises the oversized-line drain path.
	//   Line 3: another valid assistant entry with a different message.id

	line1 := `{"sessionId":"sess_ovs_1","type":"assistant","timestamp":"2026-07-07T10:00:00Z","isSidechain":false,"message":{"id":"msg_ovs_001","type":"assistant","model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	// Line 2: contains "usage": to survive phase-1 filter, but is > smallBuf bytes.
	line2Header := `{"sessionId":"sess_ovs_1","type":"assistant","timestamp":"2026-07-07T10:01:00Z","isSidechain":false,"usage":true,"PADDING":"`
	line2Pad := strings.Repeat("x", smallBuf+200)
	line2 := line2Header + line2Pad + `"}`
	line3 := `{"sessionId":"sess_ovs_1","type":"assistant","timestamp":"2026-07-07T11:00:00Z","isSidechain":false,"message":{"id":"msg_ovs_002","type":"assistant","model":"claude-opus-4-8","usage":{"input_tokens":200,"output_tokens":75,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`

	dir := t.TempDir()
	fp := filepath.Join(dir, "oversized.jsonl")
	content := line1 + "\n" + line2 + "\n" + line3 + "\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	dedup := make(map[string]resolvedEntry)
	if err := parseFileWithMaxLine(fp, time.UTC, dedup, smallBuf); err != nil {
		t.Fatalf("parseFileWithMaxLine: %v", err)
	}

	// Both valid entries (line 1 and line 3) must have been parsed.
	// Line 2 is oversized and must be skipped (the malformed JSON would also
	// be skipped on parse failure, but the drain path is what we're testing).
	if len(dedup) != 2 {
		t.Errorf("dedup len = %d, want 2 (oversized line skipped, valid entries parsed)", len(dedup))
		for k := range dedup {
			t.Logf("  dedup key: %q", k)
		}
	}
	if _, ok := dedup["msg_ovs_001"]; !ok {
		t.Error("msg_ovs_001 (before oversized line) must be parsed")
	}
	if _, ok := dedup["msg_ovs_002"]; !ok {
		t.Error("msg_ovs_002 (after oversized line) must be parsed — drain must not stop parsing")
	}
}

// ── Fix 7: empty message.id entries counted without deduplication ─────────────

// TestParseFile_EmptyID_CountedWithoutDedup verifies that entries with an empty
// message.id are each counted as a distinct entry rather than merged:
//   - Two empty-ID entries must produce two counted messages (not one).
//   - A non-empty-ID entry remains unaffected.
//
// An empty key must never cause two distinct entries to deduplicate against each
// other — each empty-ID line stands alone.
func TestParseFile_EmptyID_CountedWithoutDedup(t *testing.T) {
	dir := t.TempDir()
	// Copy the fixture containing two empty-ID entries + one real-ID entry.
	src, err := os.ReadFile(filepath.Join("testdata", "empty_id_entries.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fp := filepath.Join(dir, "empty_id_entries.jsonl")
	if err := os.WriteFile(fp, src, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	dedup := make(map[string]resolvedEntry)
	if err := parseFileWithMaxLine(fp, time.UTC, dedup, DefaultMaxLineSize); err != nil {
		t.Fatalf("parseFileWithMaxLine: %v", err)
	}

	// 2 empty-ID entries + 1 real-ID entry = 3 total distinct entries.
	if len(dedup) != 3 {
		t.Errorf("dedup len = %d, want 3 (2 empty-ID + 1 real-ID)", len(dedup))
		for k, v := range dedup {
			t.Logf("  key=%q input=%d", k, v.u.Input)
		}
	}

	// The real-ID entry must be under its actual key.
	if _, ok := dedup["msg_eid_003"]; !ok {
		t.Error("real-ID entry 'msg_eid_003' must be present in dedup")
	}

	// Total input tokens: 100 + 200 + 50 = 350.
	var totalInput int
	for _, e := range dedup {
		totalInput += e.u.Input
	}
	if totalInput != 350 {
		t.Errorf("total input tokens = %d, want 350 (100+200+50 — no empty-ID merging)", totalInput)
	}
}

// TestParseFile_EmptyID_SyntheticKeyUniqueness verifies that synthetic keys for
// empty-ID entries are unique even when the same file has many such entries.
func TestParseFile_EmptyID_SyntheticKeyUniqueness(t *testing.T) {
	const n = 10
	dir := t.TempDir()

	// Build a fixture with n empty-ID entries.
	var lines []string
	for i := 0; i < n; i++ {
		ts := fmt.Sprintf("2026-07-07T%02d:00:00Z", 10+i)
		lines = append(lines, fmt.Sprintf(
			`{"sessionId":"sess_uniq","type":"assistant","timestamp":%q,"isSidechain":false,"message":{"id":"","type":"assistant","model":"claude-opus-4-8","usage":{"input_tokens":%d,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
			ts, (i+1)*10,
		))
	}
	content := strings.Join(lines, "\n") + "\n"
	fp := filepath.Join(dir, "many_empty_ids.jsonl")
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	dedup := make(map[string]resolvedEntry)
	if err := parseFileWithMaxLine(fp, time.UTC, dedup, DefaultMaxLineSize); err != nil {
		t.Fatalf("parseFileWithMaxLine: %v", err)
	}

	if len(dedup) != n {
		t.Errorf("dedup len = %d, want %d (each empty-ID entry is distinct)", len(dedup), n)
	}
}
