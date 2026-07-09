// Package transcript discovers, parses, and aggregates Claude JSONL transcript
// files into a Stats summary used by the dashboard and waybar output.
//
// Key design decisions:
//   - Files are discovered via filepath.WalkDir (recursive, *.jsonl only).
//   - Parsing uses bufio.Reader with a configurable max-line-size. Lines that
//     exceed the limit are drained and skipped rather than aborting the rest of
//     the file (bufio.Scanner stops on ErrTooLong; we continue instead).
//   - Two-phase rejection: a fast bytes.Contains scan for "usage": precedes
//     JSON unmarshal so non-usage lines (human turns, metadata) are skipped
//     without a full parse.
//   - Dedup by message.id: isSidechain=false beats isSidechain=true; on a tie
//     keep the entry with the higher total token count.
//   - Entries with an EMPTY message.id are counted without deduplication —
//     each such line stands alone. An empty key must never cause two distinct
//     entries to deduplicate against each other.
//   - Day grouping uses the caller-provided *time.Location so that entries
//     near midnight are correctly assigned in the user's local timezone.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"clauchy/internal/pricing"
)

// ── Public types ──────────────────────────────────────────────────────────────

// DayTotals holds aggregated token counts, cost, and session/message counts
// for a single local calendar day.
type DayTotals struct {
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int // 5-minute + 1-hour write buckets combined
	CacheReadTokens  int
	Cost             float64
	Sessions         int // distinct sessionId values (Today only; zero for Week)
	Messages         int // unique deduplicated entries
}

// WeekTotals holds aggregated token counts and cost for the 7-day window.
type WeekTotals struct {
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
	Cost             float64
}

// ModelUsage holds per-model aggregated token counts and cost for the 7-day window.
type ModelUsage struct {
	Model  string
	Input  int
	Output int
	Cost   float64
}

// Stats is the fully aggregated result returned by Aggregate.
type Stats struct {
	Today         DayTotals
	Week          WeekTotals
	Models7d      []ModelUsage
	Streak        int
	UnknownModels int
	PricingDate   string
	Generated     time.Time
}

// ── Internal parsing types ────────────────────────────────────────────────────

// rawEntry is a single JSONL line from a Claude transcript file.
type rawEntry struct {
	SessionID   string     `json:"sessionId"`
	Type        string     `json:"type"`
	Timestamp   string     `json:"timestamp"`
	IsSidechain bool       `json:"isSidechain"`
	Message     rawMessage `json:"message"`
}

type rawMessage struct {
	ID    string    `json:"id"`
	Type  string    `json:"type"`
	Model string    `json:"model"`
	Usage *rawUsage `json:"usage"`
}

type rawUsage struct {
	InputTokens              int               `json:"input_tokens"`
	OutputTokens             int               `json:"output_tokens"`
	CacheCreationInputTokens int               `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int               `json:"cache_read_input_tokens"`
	CacheCreation            *rawCacheCreation `json:"cache_creation"`
}

type rawCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

// resolvedEntry is a fully-parsed, cache-bucket-resolved transcript entry
// ready for dedup and aggregation.
type resolvedEntry struct {
	sessionID   string
	model       string
	localDay    time.Time // truncated to midnight in loc
	isSidechain bool
	totalTokens int // input + output + cacheWrite + cacheRead; used for dedup tie-breaking
	u           pricing.Usage
}

// DefaultMaxLineSize is the maximum JSONL line length before a line is
// considered oversized and skipped. Claude content arrays can be multi-hundred-
// KB; 4 MB is generous but bounded.
//
// This is exported so tests can override it via a test-only helper without
// needing large in-memory fixtures.
const DefaultMaxLineSize = 4 * 1024 * 1024

// usageMarker is the byte sequence used by the two-phase scan to detect lines
// that may carry usage data before attempting a full JSON parse.
var usageMarker = []byte(`"usage":`)

// syntheticModel is the sentinel model name used by Claude Code for local
// error-placeholder entries that are not real API calls. These entries must be
// excluded from all aggregation (totals, Models7d, cost, streak, UnknownModels).
const syntheticModel = "<synthetic>"

// ── Public API ────────────────────────────────────────────────────────────────

// Aggregate walks the given roots for *.jsonl files, parses Claude transcript
// entries, deduplicates by message.id, and returns aggregated Stats.
//
// Parameters:
//   - roots: filesystem directories to walk (each walked recursively).
//   - table: pricing rate table (see pricing.Builtin / pricing.LoadOverride).
//   - now: the current instant (fixed in tests for determinism).
//   - loc: the local timezone used for day grouping.
//
// Malformed lines are skipped silently (continue-on-error). An error is only
// returned when a root directory cannot be walked at all.
func Aggregate(roots []string, table pricing.Table, now time.Time, loc *time.Location) (Stats, error) {
	today := dayStart(now, loc)
	weekStart := today.AddDate(0, 0, -6) // last 7 days including today

	// Step 1: collect all entries; dedup by message.id.
	dedup := make(map[string]resolvedEntry)
	for _, root := range roots {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // silently skip unreadable dirs
			}
			if filepath.Ext(path) != ".jsonl" {
				return nil
			}
			return parseFile(path, loc, dedup)
		}); err != nil {
			return Stats{}, fmt.Errorf("transcript walk %q: %w", root, err)
		}
	}

	// Step 2: aggregate into Stats.
	var (
		todaySessions = make(map[string]bool)
		modelAccum    = make(map[string]*modelAcc)
		daysWithMsgs  = make(map[string]bool)
		unknownModels = make(map[string]bool)
		todayTotals   DayTotals
		weekTotals    WeekTotals
	)

	for _, e := range dedup {
		dayKey := e.localDay.Format("2006-01-02")
		daysWithMsgs[dayKey] = true

		// localDay, today, and weekStart are all day-starts in the same loc so
		// Equal is safe and more readable than a half-open range here.
		isToday := e.localDay.Equal(today)
		isInWeek := !e.localDay.Before(weekStart) && !e.localDay.After(today)

		rate, ok := pricing.RateFor(table, e.model)
		if !ok {
			unknownModels[e.model] = true
		}
		cost := pricing.Cost(e.u, rate)

		if isToday {
			todayTotals.InputTokens += e.u.Input
			todayTotals.OutputTokens += e.u.Output
			todayTotals.CacheWriteTokens += e.u.CacheWrite5m + e.u.CacheWrite1h
			todayTotals.CacheReadTokens += e.u.CacheRead
			todayTotals.Cost += cost
			todayTotals.Messages++
			todaySessions[e.sessionID] = true
		}

		if isInWeek {
			weekTotals.InputTokens += e.u.Input
			weekTotals.OutputTokens += e.u.Output
			weekTotals.CacheWriteTokens += e.u.CacheWrite5m + e.u.CacheWrite1h
			weekTotals.CacheReadTokens += e.u.CacheRead
			weekTotals.Cost += cost

			// Per-model accumulation for Models7d.
			acc, found := modelAccum[e.model]
			if !found {
				acc = &modelAcc{}
				modelAccum[e.model] = acc
			}
			acc.input += e.u.Input
			acc.output += e.u.Output
			acc.cost += cost
		}
	}

	todayTotals.Sessions = len(todaySessions)

	// Build Models7d slice. Go map iteration order is randomised, so an
	// explicit sort is required for a stable UI. Sort: total tokens DESC,
	// model name ASC as the tiebreaker.
	models7d := make([]ModelUsage, 0, len(modelAccum))
	for model, acc := range modelAccum {
		models7d = append(models7d, ModelUsage{
			Model:  model,
			Input:  acc.input,
			Output: acc.output,
			Cost:   acc.cost,
		})
	}
	sort.Slice(models7d, func(i, j int) bool {
		ti := models7d[i].Input + models7d[i].Output
		tj := models7d[j].Input + models7d[j].Output
		if ti != tj {
			return ti > tj // higher total first
		}
		return models7d[i].Model < models7d[j].Model // name asc on tie
	})

	return Stats{
		Today:         todayTotals,
		Week:          weekTotals,
		Models7d:      models7d,
		Streak:        calculateStreak(daysWithMsgs, today),
		UnknownModels: len(unknownModels),
		PricingDate:   pricing.PricingDate,
		Generated:     now,
	}, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// modelAcc accumulates per-model token counts across the 7-day window.
type modelAcc struct {
	input, output int
	cost          float64
}

// dayStart returns the start of the local calendar day containing t.
func dayStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// calculateStreak counts consecutive local days with ≥1 usage-bearing message
// counting backwards from an anchor:
//   - anchor = today when today has messages (GitHub/Duolingo rule).
//   - anchor = yesterday when today is empty (grace day: today being empty does
//     not break the streak from prior days).
func calculateStreak(days map[string]bool, today time.Time) int {
	anchor := today
	if !days[today.Format("2006-01-02")] {
		anchor = today.AddDate(0, 0, -1) // grace: count from yesterday
	}

	streak := 0
	d := anchor
	for {
		if !days[d.Format("2006-01-02")] {
			break
		}
		streak++
		d = d.AddDate(0, 0, -1)
	}
	return streak
}

// parseFile reads a single JSONL file and merges parsed entries into dedup.
// It delegates to parseFileWithMaxLine using the production max-line size.
func parseFile(path string, loc *time.Location, dedup map[string]resolvedEntry) error {
	return parseFileWithMaxLine(path, loc, dedup, DefaultMaxLineSize)
}

// parseFileWithMaxLine is the injectable version of parseFile used by tests
// that need a smaller max-line budget to avoid large in-memory fixtures.
//
// Lines that exceed maxLineBytes are drained (consumed byte-by-byte until the
// next newline) and SKIPPED — they do not terminate parsing of subsequent lines.
// This fixes the ErrTooLong-stops-scanning bug with bufio.Scanner.
//
// Entries with an empty message.id are counted WITHOUT deduplication: each
// such line is stored under a unique synthetic key so it stands alone. An empty
// key must never cause two distinct entries to merge against each other.
func parseFileWithMaxLine(path string, loc *time.Location, dedup map[string]resolvedEntry, maxLineBytes int) error {
	f, err := os.Open(path)
	if err != nil {
		return nil // unreadable file: silently skip
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, maxLineBytes)
	// noEmptyIDCount tracks how many empty-ID lines have been seen in this file.
	// It is used to generate unique synthetic dedup keys for empty-ID entries.
	noEmptyIDCount := 0

	for {
		line, isPrefix, err := reader.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			}
			// Any other I/O error: stop processing this file.
			break
		}

		if isPrefix {
			// Line exceeds the buffer size — drain remaining bytes until newline,
			// then skip this line. Subsequent lines are unaffected.
			for isPrefix {
				_, isPrefix, err = reader.ReadLine()
				if err != nil {
					break
				}
			}
			continue // oversized line skipped; continue with next line
		}

		if len(line) == 0 {
			continue
		}

		// Phase 1: fast byte scan — skip lines that cannot have usage data.
		if !bytes.Contains(line, usageMarker) {
			continue
		}

		// Phase 2: full JSON parse.
		var e rawEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // malformed JSON: skip silently
		}

		// Only assistant entries with usage data are counted.
		if e.Type != "assistant" || e.Message.Usage == nil {
			continue
		}

		// Exclude Claude Code local error-placeholder entries. These are not
		// real API calls and must not affect any aggregation field.
		if e.Message.Model == syntheticModel {
			continue
		}

		ts, err := parseTimestamp(e.Timestamp)
		if err != nil {
			continue
		}
		localDay := dayStart(ts, loc)

		u := resolveUsage(e.Message.Usage)
		total := u.Input + u.Output + u.CacheWrite5m + u.CacheWrite1h + u.CacheRead

		candidate := resolvedEntry{
			sessionID:   e.SessionID,
			model:       e.Message.Model,
			localDay:    localDay,
			isSidechain: e.IsSidechain,
			totalTokens: total,
			u:           u,
		}

		msgID := e.Message.ID
		if msgID == "" {
			// Empty message.id: count without deduplication. Each empty-ID entry
			// stands alone — a synthetic unique key prevents merging distinct
			// entries against each other. The key is file-path-scoped.
			noEmptyIDCount++
			msgID = fmt.Sprintf("\x00empty:%s:%d", path, noEmptyIDCount)
			dedup[msgID] = candidate
			continue
		}

		existing, seen := dedup[msgID]
		if !seen {
			dedup[msgID] = candidate
			continue
		}

		// Dedup rule: prefer isSidechain=false; on tie keep higher total token count.
		switch {
		case existing.isSidechain && !candidate.isSidechain:
			dedup[msgID] = candidate // parent beats subagent
		case !existing.isSidechain && candidate.isSidechain:
			// keep existing parent — do nothing
		default:
			// tie: both same isSidechain value
			if candidate.totalTokens > existing.totalTokens {
				dedup[msgID] = candidate
			}
		}
	}

	return nil
}

// resolveUsage converts a raw API usage record into the pricing.Usage bucket
// layout used by pricing.Cost.
//
// Cache-write split rule (from design):
//   - When cache_creation sub-object is present: use ephemeral_5m and ephemeral_1h.
//   - When absent: use cache_creation_input_tokens as the 5-minute bucket (fallback).
func resolveUsage(u *rawUsage) pricing.Usage {
	p := pricing.Usage{
		Input:     u.InputTokens,
		Output:    u.OutputTokens,
		CacheRead: u.CacheReadInputTokens,
	}

	if u.CacheCreation != nil {
		p.CacheWrite5m = u.CacheCreation.Ephemeral5mInputTokens
		p.CacheWrite1h = u.CacheCreation.Ephemeral1hInputTokens
	} else {
		p.CacheWrite5m = u.CacheCreationInputTokens // fallback: entire amount to 5m bucket
	}

	return p
}

// parseTimestamp parses a Claude transcript timestamp in RFC3339 or
// RFC3339Nano format. In Go, time.RFC3339Nano accepts both formats —
// fractional-second fields are optional in parsing, so a single format
// string covers all real-world Claude transcript timestamps.
func parseTimestamp(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("transcript: cannot parse timestamp %q: %w", s, err)
	}
	return t, nil
}
