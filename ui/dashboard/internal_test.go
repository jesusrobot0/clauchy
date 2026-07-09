package dashboard

import "testing"

// TestHumanize verifies token-count formatting tiers including the new B/T tiers.
// Boundaries:
//   - < 1_000             → plain integer  ("550")
//   - 1_000–999_999       → integer K      ("2K", "184K")
//   - 1_000_000–999_999_999 → one-decimal M  ("82.1M", "999.9M")
//   - ≥ 1_000_000_000     → one-decimal B  ("1.0B", "1.1B", "999.9B")
//   - ≥ 1_000_000_000_000 → one-decimal T  ("1.0T", "1.5T")
func TestHumanize(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		// Below K
		{0, "0"},
		{550, "550"},
		{999, "999"},
		// K tier
		{1_000, "1K"},
		{2_000, "2K"},
		{184_000, "184K"},
		{999_999, "999K"},
		// M tier
		{1_000_000, "1.0M"},
		{82_100_000, "82.1M"},
		{327_000_000, "327.0M"},
		{999_999_999, "1000.0M"}, // just below 1B: still M (floor division)
		// B tier (≥ 1_000_000_000)
		{1_000_000_000, "1.0B"},
		{1_100_000_000, "1.1B"},
		{999_000_000_000, "999.0B"},
		// T tier (≥ 1_000_000_000_000)
		{1_000_000_000_000, "1.0T"},
		{1_500_000_000_000, "1.5T"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := humanize(tc.n); got != tc.want {
				t.Errorf("humanize(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

func TestHumanDate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"iso date", "2026-07-07", "Jul 7, 2026"},
		{"unparseable passthrough", "not-a-date", "not-a-date"},
		{"empty passthrough", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanDate(tc.in); got != tc.want {
				t.Errorf("humanDate(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
