// Package install idempotently adds the custom/clauchy Waybar module and CSS
// color block to the user's Waybar configuration files, writes severity-colored
// SVG icon variants to the data directory, and appends Hyprland window rules
// so the dashboard panel floats above other windows.
//
// Key design decisions:
//   - A JSONC-aware tokenizer (string-state + comment-state DFA) prevents
//     braces inside strings or comments from affecting depth tracking.
//   - All five install conditions (exec, array entry, return-type/interval,
//     on-click state, CSS marker) are checked independently; only the missing
//     pieces are repaired, never an already-correct piece.
//   - Non-clobbering backups (.bak.<epoch>[.N]) guarantee no prior backup is
//     ever overwritten.
//   - ErrAmbiguousConfig aborts without writing either file.
//   - A deliberately-absent on-click (OnClickResolved:false) is an accepted
//     terminal-less state; re-runs treat it as a full no-op.
//   - Icon variants are always regenerated on every run (overwrite-safe, content-
//     compare for idempotency reporting is not done — regeneration is cheap and
//     stateless).
//   - Hyprland: missing hyprland.conf emits a warning and is skipped; it does not
//     abort the install (non-Hyprland setups still get the Waybar module).
package install

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed assets/claude.svg
var claudeSVG []byte

// Sentinel errors returned by Run.
var (
	ErrConfigNotFound  = errors.New("install: config.jsonc not found")
	ErrNoModulesArray  = errors.New("install: no modules-* array found in config")
	ErrAmbiguousConfig = errors.New("install: cannot reliably parse config; aborting without writing")
)

// Reloader is called after any file change so Waybar / Hyprland pick up the
// new config. A default implementation is wired by main; tests inject a no-op.
type Reloader func() error

// RunConfig holds all parameters for Run.
// Callers build this struct instead of a long positional argument list.
type RunConfig struct {
	// ConfigPath is the path to ~/.config/waybar/config.jsonc.
	ConfigPath string
	// StylePath is the path to ~/.config/waybar/style.css.
	StylePath string
	// DataHome is $XDG_DATA_HOME (or ~/.local/share). Icon variants are written
	// to DataHome/clauchy/ on each run. When empty, icon writing is silently
	// skipped; the rest of the install (config, CSS, Hyprland) proceeds normally.
	DataHome string
	// HyprlandConf is the path to ~/.config/hypr/hyprland.conf.
	// Empty string means skip Hyprland processing entirely (non-Hyprland setup).
	// A non-empty path that does not exist produces a warning and is skipped.
	HyprlandConf string
	// Colorful, when true, appends --colorful to the on-click command so the
	// dashboard opens in colorful mode. When false (default) the dashboard opens
	// in monochrome mode (the default). Changing this flag between runs is treated
	// as a stale on-click and triggers the existing repair path.
	Colorful bool
	// Reload is called after any configuration file is changed.
	Reload Reloader
}

// Result reports what Run did this invocation.
type Result struct {
	ConfigChanged   bool
	CSSChanged      bool
	IconsWritten    bool // true when icon variant SVGs were (re)written
	HyprChanged     bool // true when hyprland.conf was modified
	Backups         []string
	OnClickResolved bool
	Warnings        []string
}

// panelClass is the WM class used both in the on-click command and in
// Hyprland window rules. A single constant keeps them in sync.
const panelClass = "clauchy.panel"

// PanelClass returns the WM window class used for the floating dashboard panel.
// Exported for cross-package tests and verification.
func PanelClass() string { return panelClass }

// iconColors holds the fill hex for each severity variant.
// Low uses white (#ffffff) to match the other white bar icons — the calm state
// is now white, consistent with bar icon conventions. Brand orange is no longer
// used for low severity.
// Mid / High / Critical use the theme palette hex values for severity coloring.
// This is the single source of truth within the install package;
// ui/theme's cross-consistency test imports this map to check alignment.
var iconColors = map[string]string{
	"low":      "#ffffff", // white — matches other white bar icons in the calm state
	"mid":      "#f9e2af", // Catppuccin Mocha Yellow
	"high":     "#fab387", // Catppuccin Mocha Peach
	"critical": "#f38ba8", // Catppuccin Mocha Red
}

// IconSeverityColors returns the icon fill hex values by severity key.
// Exported for cross-package tests (ui/theme cross-consistency).
func IconSeverityColors() map[string]string {
	out := make(map[string]string, len(iconColors))
	for k, v := range iconColors {
		out[k] = v
	}
	return out
}

// knownTerminals is the allow-list of terminals that accept --class and -e.
var knownTerminals = []string{"ghostty", "alacritty", "kitty", "foot"}

// CSS marker constants.
const (
	cssMarkerStart = "/* clauchy start */"
	cssMarkerEnd   = "/* clauchy end */"
)

// buildCSSBlock returns the Waybar CSS block that clauchy install generates.
// It embeds absolute paths to the icon SVG files so Waybar can load them as
// background images. The icon glyph is replaced by CSS background-image so
// the module box can be any size; Output.Text emits a single space " " to
// make the box exist without drawing a glyph.
//
// Each severity class gets a background-image override pointing at its icon
// file. The base #custom-clauchy selector sets background-size/repeat/position
// and a min-width so the box is always wide enough to show the image.
func buildCSSBlock(iconDir string) string {
	lowPath := filepath.Join(iconDir, "icon-low.svg")
	midPath := filepath.Join(iconDir, "icon-mid.svg")
	highPath := filepath.Join(iconDir, "icon-high.svg")
	critPath := filepath.Join(iconDir, "icon-critical.svg")

	return `/* clauchy start */
#custom-clauchy {
    background-image: url("` + lowPath + `");
    background-size: 14px 14px;
    background-repeat: no-repeat;
    background-position: center;
    min-width: 18px;
}
#custom-clauchy.mid {
    background-image: url("` + midPath + `");
}
#custom-clauchy.high {
    background-image: url("` + highPath + `");
}
#custom-clauchy.critical {
    background-image: url("` + critPath + `");
}
/* clauchy end */
`
}

// ─── JSONC tokenizer ──────────────────────────────────────────────────────────

type tokKind int

const (
	tkString tokKind = iota // "..."
	tkNumber                // 0-9, -
	tkIdent                 // true, false, null
	tkColon
	tkComma
	tkLBrace
	tkRBrace
	tkLBracket
	tkRBracket
)

// jsToken is a JSONC token with its byte position in the source and the nesting
// depth AFTER the token is processed.
type jsToken struct {
	kind  tokKind
	value string // set for tkString, tkNumber, tkIdent
	start int    // inclusive byte offset
	end   int    // exclusive byte offset
	depth int    // nesting depth after this token
}

// tokenizeJSONC produces a token stream from JSONC source.
// The DFA tracks six states so that braces and brackets inside string literals,
// // line comments, and /* */ block comments are silently ignored — they do not
// contribute to the depth counter.
func tokenizeJSONC(src []byte) ([]jsToken, error) {
	type state int
	const (
		sNormal state = iota
		sString
		sStringEscape
		sSlash
		sLineComment
		sBlockComment
		sBlockCommentStar
		sIdent
		sNumber
	)

	tokens := make([]jsToken, 0, 128)
	depth := 0
	st := sNormal
	var tokStart int
	var buf strings.Builder

	flush := func(kind tokKind, end int) {
		tokens = append(tokens, jsToken{
			kind:  kind,
			value: buf.String(),
			start: tokStart,
			end:   end,
			depth: depth,
		})
		buf.Reset()
	}

	n := len(src)
	for i := 0; i < n; i++ {
		b := src[i]
		switch st {
		case sNormal:
			switch {
			case b == '"':
				tokStart = i
				buf.Reset()
				st = sString
			case b == '{':
				depth++
				tokens = append(tokens, jsToken{kind: tkLBrace, start: i, end: i + 1, depth: depth})
			case b == '}':
				depth--
				tokens = append(tokens, jsToken{kind: tkRBrace, start: i, end: i + 1, depth: depth})
			case b == '[':
				depth++
				tokens = append(tokens, jsToken{kind: tkLBracket, start: i, end: i + 1, depth: depth})
			case b == ']':
				depth--
				tokens = append(tokens, jsToken{kind: tkRBracket, start: i, end: i + 1, depth: depth})
			case b == ':':
				tokens = append(tokens, jsToken{kind: tkColon, start: i, end: i + 1, depth: depth})
			case b == ',':
				tokens = append(tokens, jsToken{kind: tkComma, start: i, end: i + 1, depth: depth})
			case b == '/':
				st = sSlash
			case b == 't' || b == 'f' || b == 'n':
				tokStart = i
				buf.Reset()
				buf.WriteByte(b)
				st = sIdent
			case b == '-' || (b >= '0' && b <= '9'):
				tokStart = i
				buf.Reset()
				buf.WriteByte(b)
				st = sNumber
			}
		case sString:
			switch b {
			case '\\':
				buf.WriteByte(b)
				st = sStringEscape
			case '"':
				inner := buf.String()
				tokens = append(tokens, jsToken{kind: tkString, value: inner, start: tokStart, end: i + 1, depth: depth})
				buf.Reset()
				st = sNormal
			default:
				buf.WriteByte(b)
			}
		case sStringEscape:
			buf.WriteByte(b)
			st = sString
		case sSlash:
			switch b {
			case '/':
				st = sLineComment
			case '*':
				st = sBlockComment
			default:
				st = sNormal
				i-- // re-process
			}
		case sLineComment:
			if b == '\n' {
				st = sNormal
			}
		case sBlockComment:
			if b == '*' {
				st = sBlockCommentStar
			}
		case sBlockCommentStar:
			if b == '/' {
				st = sNormal
			} else if b != '*' {
				st = sBlockComment
			}
		case sIdent:
			if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
				buf.WriteByte(b)
			} else {
				flush(tkIdent, i)
				st = sNormal
				i--
			}
		case sNumber:
			if (b >= '0' && b <= '9') || b == '.' || b == 'e' || b == 'E' || b == '+' || b == '-' {
				buf.WriteByte(b)
			} else {
				flush(tkNumber, i)
				st = sNormal
				i--
			}
		}
	}
	if st == sIdent {
		flush(tkIdent, n)
	} else if st == sNumber {
		flush(tkNumber, n)
	}
	return tokens, nil
}

// findValueEnd returns (exclusive end byte offset, index of last token) for
// the JSON value starting at tokens[startIdx].
//
// Depth invariant:
//   - A tkLBrace token has depth D (inside the new block).
//   - Its matching tkRBrace has depth D-1 (outside again).
//   - Same for tkLBracket / tkRBracket.
func findValueEnd(tokens []jsToken, startIdx int) (int, int, error) {
	if startIdx >= len(tokens) {
		return 0, 0, fmt.Errorf("expected value token at index %d", startIdx)
	}
	tok := tokens[startIdx]
	switch tok.kind {
	case tkString, tkNumber, tkIdent:
		return tok.end, startIdx, nil
	case tkLBrace:
		target := tok.depth - 1
		for j := startIdx + 1; j < len(tokens); j++ {
			if tokens[j].kind == tkRBrace && tokens[j].depth == target {
				return tokens[j].end, j, nil
			}
		}
		return 0, 0, fmt.Errorf("unmatched { at byte %d", tok.start)
	case tkLBracket:
		target := tok.depth - 1
		for j := startIdx + 1; j < len(tokens); j++ {
			if tokens[j].kind == tkRBracket && tokens[j].depth == target {
				return tokens[j].end, j, nil
			}
		}
		return 0, 0, fmt.Errorf("unmatched [ at byte %d", tok.start)
	}
	return 0, 0, fmt.Errorf("unexpected token kind %d at byte %d", tok.kind, tok.start)
}

// ─── Config scanner ───────────────────────────────────────────────────────────

// modulesEntry holds information about a discovered modules-* array.
type modulesEntry struct {
	name       string
	arrayClose int // byte position of ']'
	hasClauchy bool
}

// configScan holds everything the editor needs to know about the current state.
type configScan struct {
	clauchy struct {
		found       bool
		exec        string
		hasRetType  bool
		hasInterval bool
		onClick     string // empty when the key is absent

		keyStart int // byte offset of opening '"' of '"custom/clauchy"' key
		valueEnd int // exclusive byte offset past the closing '}' of the value
	}

	modules         []modulesEntry
	siblingOnClicks []string // on-click values from non-clauchy module objects

	rootClosePos int // byte position of root '}'
}

// hasAnyModules returns true when at least one modules-* array was found.
func (sc *configScan) hasAnyModules() bool { return len(sc.modules) > 0 }

// targetModules returns the preferred modules array (modules-right first, else first).
func (sc *configScan) targetModules() *modulesEntry {
	for i := range sc.modules {
		if sc.modules[i].name == "modules-right" {
			return &sc.modules[i]
		}
	}
	if len(sc.modules) > 0 {
		return &sc.modules[0]
	}
	return nil
}

// clauchy is in at least one array
func (sc *configScan) clauchyInAnyArray() bool {
	for _, m := range sc.modules {
		if m.hasClauchy {
			return true
		}
	}
	return false
}

// scanConfig walks the token stream to populate a configScan.
func scanConfig(tokens []jsToken, src []byte) (*configScan, error) {
	if len(tokens) == 0 {
		return nil, ErrAmbiguousConfig
	}
	sc := &configScan{}

	// Find root } (the RBrace with depth==0)
	for j := len(tokens) - 1; j >= 0; j-- {
		if tokens[j].kind == tkRBrace && tokens[j].depth == 0 {
			sc.rootClosePos = tokens[j].start
			break
		}
	}
	if sc.rootClosePos == 0 && (len(tokens) == 0 || tokens[0].kind != tkLBrace) {
		return nil, ErrAmbiguousConfig
	}

	n := len(tokens)
	i := 0
	// Skip root {
	if i < n && tokens[i].kind == tkLBrace && tokens[i].depth == 1 {
		i++
	}

	for i < n {
		tok := tokens[i]

		// Look for a top-level string key: tkString at depth==1 followed by tkColon at depth==1
		if tok.kind != tkString || tok.depth != 1 {
			i++
			continue
		}
		if i+1 >= n || tokens[i+1].kind != tkColon || tokens[i+1].depth != 1 {
			i++
			continue
		}
		if i+2 >= n {
			break
		}

		key := tok.value
		keyStart := tok.start

		valEnd, endIdx, err := findValueEnd(tokens, i+2)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrAmbiguousConfig, err)
		}

		valTok := tokens[i+2]

		switch {
		case key == "custom/clauchy":
			sc.clauchy.found = true
			sc.clauchy.keyStart = keyStart
			sc.clauchy.valueEnd = valEnd

			if valTok.kind == tkLBrace {
				// Scan inner keys at depth == valTok.depth
				innerDepth := valTok.depth
				for j := i + 3; j < endIdx; j++ {
					inner := tokens[j]
					if inner.kind != tkString || inner.depth != innerDepth {
						continue
					}
					// Check key → colon → value pattern
					if j+2 >= endIdx || tokens[j+1].kind != tkColon {
						continue
					}
					innerVal := tokens[j+2]
					switch inner.value {
					case "exec":
						if innerVal.kind == tkString {
							sc.clauchy.exec = innerVal.value
						}
					case "return-type":
						if innerVal.kind == tkString && innerVal.value == "json" {
							sc.clauchy.hasRetType = true
						}
					case "interval":
						if innerVal.kind == tkNumber && innerVal.value == "60" {
							sc.clauchy.hasInterval = true
						}
					case "on-click":
						if innerVal.kind == tkString {
							sc.clauchy.onClick = innerVal.value
						}
					}
				}
			}

		case strings.HasPrefix(key, "modules-"):
			entry := modulesEntry{name: key}
			if valTok.kind == tkLBracket {
				entry.arrayClose = tokens[endIdx].start
				innerDepth := valTok.depth
				for j := i + 3; j < endIdx; j++ {
					if tokens[j].kind == tkString && tokens[j].depth == innerDepth &&
						tokens[j].value == "custom/clauchy" {
						entry.hasClauchy = true
					}
				}
			}
			sc.modules = append(sc.modules, entry)

		default:
			// Collect on-click values from sibling module objects for terminal resolution
			if valTok.kind == tkLBrace {
				innerDepth := valTok.depth
				for j := i + 3; j < endIdx; j++ {
					inner := tokens[j]
					if inner.kind != tkString || inner.depth != innerDepth || inner.value != "on-click" {
						continue
					}
					if j+2 < endIdx && tokens[j+2].kind == tkString {
						sc.siblingOnClicks = append(sc.siblingOnClicks, tokens[j+2].value)
					}
				}
			}
		}

		i = endIdx + 1
	}

	return sc, nil
}

// ─── Terminal resolution ──────────────────────────────────────────────────────

// resolveTerminal returns the first known terminal binary it can find, following
// the documented priority:
//  1. A sibling module's on-click whose command starts with a known terminal
//  2. $TERMINAL env var (only if its basename is in the allow-list)
//  3. PATH probe of the allow-list in order
func resolveTerminal(siblingOnClicks []string) string {
	// Priority 1: sibling on-click
	for _, s := range siblingOnClicks {
		if t := terminalFromOnClick(s); t != "" {
			return t
		}
	}
	// Priority 2: $TERMINAL
	if t := os.Getenv("TERMINAL"); t != "" {
		base := filepath.Base(t)
		if isKnownTerminal(base) {
			return base
		}
	}
	// Priority 3: PATH probe
	for _, t := range knownTerminals {
		if _, err := exec.LookPath(t); err == nil {
			return t
		}
	}
	return ""
}

// terminalFromOnClick extracts the terminal binary from an on-click value that
// matches the pattern "uwsm-app -- <terminal> ...". Returns "" if the pattern
// does not match or the terminal is not in the allow-list.
func terminalFromOnClick(s string) string {
	const prefix = "uwsm-app -- "
	if !strings.HasPrefix(s, prefix) {
		return ""
	}
	rest := s[len(prefix):]
	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return ""
	}
	t := filepath.Base(parts[0])
	if isKnownTerminal(t) {
		return t
	}
	return ""
}

func isKnownTerminal(t string) bool {
	for _, k := range knownTerminals {
		if t == k {
			return true
		}
	}
	return false
}

// buildOnClickCmd returns the full on-click command for the given terminal.
// Each known terminal receives a font-size flag so the floating panel renders
// with a comfortable reading size (the panel is clauchy's own window).
// foot: foot's --override flag requires an explicit font-name pattern
// (e.g. "Monospace:size=14"); omitting it is safer than assuming a font name
// on behalf of the user, so foot gets no font-size override.
//
// When colorful is true the on-click launches "clauchy --colorful" so the
// dashboard opens in colorful mode. Changing this flag between runs is treated
// as a stale on-click and triggers the idempotency repair path.
func buildOnClickCmd(terminal string, colorful bool) string {
	var fontFlag string
	switch terminal {
	case "ghostty":
		fontFlag = "--font-size=9.5"
	case "alacritty":
		fontFlag = "-o font.size=9.5"
	case "kitty":
		fontFlag = "-o font_size=9.5"
		// foot: no font-size flag — see comment above.
	}
	cmd := "uwsm-app -- " + terminal + " --class=" + panelClass
	if fontFlag != "" {
		cmd = "uwsm-app -- " + terminal + " " + fontFlag + " --class=" + panelClass
	}
	if colorful {
		return cmd + " -e clauchy --colorful"
	}
	return cmd + " -e clauchy"
}

// ─── Config editor ────────────────────────────────────────────────────────────

// edit is a surgical replacement: src[start:end] → replace.
// When start == end, it is an insertion at that position.
type edit struct {
	start   int
	end     int
	replace string
}

// applyEdits applies a set of non-overlapping edits to src (sorted by position).
func applyEdits(src []byte, edits []edit) []byte {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out bytes.Buffer
	pos := 0
	for _, e := range edits {
		if e.start > pos {
			out.Write(src[pos:e.start])
		}
		out.WriteString(e.replace)
		pos = e.end
	}
	out.Write(src[pos:])
	return out.Bytes()
}

// lastNonWSBefore returns the index of the last non-whitespace byte strictly
// before pos, or -1 if none.
func lastNonWSBefore(src []byte, pos int) int {
	for i := pos - 1; i >= 0; i-- {
		b := src[i]
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return i
		}
	}
	return -1
}

// lineIndentBefore returns the leading whitespace of the line containing pos.
func lineIndentBefore(src []byte, pos int) string {
	lineStart := pos
	for lineStart > 0 && src[lineStart-1] != '\n' {
		lineStart--
	}
	var ind []byte
	for _, b := range src[lineStart:pos] {
		if b == ' ' || b == '\t' {
			ind = append(ind, b)
		} else {
			break
		}
	}
	return string(ind)
}

// buildValueBlock returns the { ... } part of the custom/clauchy module.
// onClickCmd is the full on-click command string; pass "" for terminal-less.
func buildValueBlock(onClickCmd string) string {
	var sb strings.Builder
	sb.WriteString("{\n")
	sb.WriteString(`        "exec": "clauchy waybar",` + "\n")
	sb.WriteString(`        "return-type": "json",` + "\n")
	sb.WriteString(`        "interval": 60`)
	if onClickCmd != "" {
		sb.WriteString(",\n")
		sb.WriteString(`        "on-click": "` + onClickCmd + `"`)
	}
	sb.WriteString("\n    }")
	return sb.String()
}

// insertModuleEdit returns an edit that inserts the new member before rootClosePos.
func insertModuleEdit(src []byte, rootClosePos int, memberText string) edit {
	lastPos := lastNonWSBefore(src, rootClosePos)
	var sb strings.Builder
	if lastPos >= 0 && src[lastPos] != ',' {
		sb.WriteByte(',')
	}
	sb.WriteString("\n    ")
	sb.WriteString(memberText)
	sb.WriteString("\n")
	return edit{start: rootClosePos, end: rootClosePos, replace: sb.String()}
}

// rewriteModuleEdit returns an edit that replaces the existing clauchy key-value block.
func rewriteModuleEdit(keyStart, valueEnd int, memberText string) edit {
	return edit{start: keyStart, end: valueEnd, replace: memberText}
}

// arrayAppendEdit returns an edit that appends "custom/clauchy" before arrayClosePos.
func arrayAppendEdit(src []byte, arrayClosePos int) edit {
	lastPos := lastNonWSBefore(src, arrayClosePos)
	closingIndent := lineIndentBefore(src, arrayClosePos)
	if strings.TrimSpace(closingIndent) != "" {
		closingIndent = "    " // fallback if ] is not at line start
	}
	itemIndent := closingIndent + "    "

	var sb strings.Builder
	if lastPos >= 0 && src[lastPos] != ',' {
		sb.WriteByte(',')
	}
	sb.WriteString("\n")
	sb.WriteString(itemIndent)
	sb.WriteString(`"custom/clauchy"`)
	sb.WriteString("\n")
	sb.WriteString(closingIndent)
	return edit{start: arrayClosePos, end: arrayClosePos, replace: sb.String()}
}

// ─── Icon helpers ─────────────────────────────────────────────────────────────

// writeIcons writes four severity-colored SVG icon variants to iconDir.
// It uses string replacement on the embedded SVG template, replacing the brand
// fill hex with the per-severity hex. icon-low.svg keeps the brand color
// (#D97757) unchanged. Existing files are overwritten (overwrite-safe).
func writeIcons(iconDir string) error {
	if err := os.MkdirAll(iconDir, 0o755); err != nil {
		return fmt.Errorf("icon dir: %w", err)
	}

	const brandHex = "#D97757"
	for severity, hex := range iconColors {
		var content []byte
		if hex == brandHex {
			content = claudeSVG
		} else {
			content = bytes.ReplaceAll(claudeSVG, []byte(brandHex), []byte(hex))
		}
		p := filepath.Join(iconDir, "icon-"+severity+".svg")
		if err := os.WriteFile(p, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}
	return nil
}

// ─── Hyprland helpers ─────────────────────────────────────────────────────────

const (
	hyprMarkerStart = "# clauchy start"
	hyprMarkerEnd   = "# clauchy end"
)

// hyprBlock returns the Hyprland window rules block to append.
// The class matches panelClass (clauchy.panel), keeping it in sync with
// buildOnClickCmd via the shared panelClass constant.
func hyprBlock() string {
	cls := panelClass
	return hyprMarkerStart + `
windowrule = float on, match:class ^` + strings.ReplaceAll(cls, ".", `\.`) + `$
windowrule = size 920 520, match:class ^` + strings.ReplaceAll(cls, ".", `\.`) + `$
windowrule = center on, match:class ^` + strings.ReplaceAll(cls, ".", `\.`) + `$
` + hyprMarkerEnd + "\n"
}

// hasHyprBlock returns true when both Hyprland markers are present and ordered.
func hasHyprBlock(src []byte) bool {
	si := bytes.Index(src, []byte(hyprMarkerStart))
	ei := bytes.Index(src, []byte(hyprMarkerEnd))
	return si >= 0 && ei > si
}

// extractHyprBlock returns the marker-delimited clauchy block, markers included,
// without a trailing newline. Callers must check hasHyprBlock first.
func extractHyprBlock(src []byte) string {
	si := bytes.Index(src, []byte(hyprMarkerStart))
	ei := bytes.Index(src, []byte(hyprMarkerEnd))
	return string(src[si : ei+len(hyprMarkerEnd)])
}

// replaceHyprBlockContent swaps the existing marker-delimited block for block,
// leaving everything around it untouched. Content-awareness mirrors the CSS
// stale-block repair: presence alone is not "installed" — stale content is repaired.
func replaceHyprBlockContent(src []byte, block string) []byte {
	si := bytes.Index(src, []byte(hyprMarkerStart))
	ei := bytes.Index(src, []byte(hyprMarkerEnd)) + len(hyprMarkerEnd)
	// Also consume a trailing newline if present so we don't leave a blank line.
	if ei < len(src) && src[ei] == '\n' {
		ei++
	}
	var out bytes.Buffer
	out.Write(src[:si])
	out.WriteString(strings.TrimRight(block, "\n"))
	out.WriteByte('\n')
	out.Write(src[ei:])
	return out.Bytes()
}

// hyprBlockUpToDate returns true when the existing block in src exactly matches
// the current hyprBlock() output. Presence of markers alone is not "up to date" —
// an older clauchy version's block content must be repaired, mirroring condE for CSS.
func hyprBlockUpToDate(src []byte) bool {
	if !hasHyprBlock(src) {
		return false
	}
	return extractHyprBlock(src) == strings.TrimRight(hyprBlock(), "\n")
}

// appendHyprBlock appends the clauchy Hyprland window rules block to src.
func appendHyprBlock(src []byte) []byte {
	var out bytes.Buffer
	out.Write(src)
	if len(src) > 0 && src[len(src)-1] != '\n' {
		out.WriteByte('\n')
	}
	out.WriteString("\n")
	out.WriteString(hyprBlock())
	return out.Bytes()
}

// ─── CSS helpers ──────────────────────────────────────────────────────────────

// validateCSSMarkers returns an error when the CSS has mismatched markers —
// one present without the other, or end before start.
func validateCSSMarkers(src []byte) error {
	si := bytes.Index(src, []byte(cssMarkerStart))
	ei := bytes.Index(src, []byte(cssMarkerEnd))
	switch {
	case si >= 0 && ei < 0:
		return fmt.Errorf("%w: CSS has %s but no matching %s", ErrAmbiguousConfig, cssMarkerStart, cssMarkerEnd)
	case si < 0 && ei >= 0:
		return fmt.Errorf("%w: CSS has %s but no matching %s", ErrAmbiguousConfig, cssMarkerEnd, cssMarkerStart)
	case si >= 0 && ei < si:
		return fmt.Errorf("%w: CSS end marker appears before start marker", ErrAmbiguousConfig)
	}
	return nil
}

// hasMarkerBlock returns true when both CSS markers are present and correctly ordered.
func hasMarkerBlock(src []byte) bool {
	si := bytes.Index(src, []byte(cssMarkerStart))
	ei := bytes.Index(src, []byte(cssMarkerEnd))
	return si >= 0 && ei > si
}

// extractCSSBlock returns the marker-delimited clauchy block, markers included,
// without a trailing newline. Callers must check hasMarkerBlock first.
func extractCSSBlock(src []byte) string {
	si := bytes.Index(src, []byte(cssMarkerStart))
	ei := bytes.Index(src, []byte(cssMarkerEnd))
	return string(src[si : ei+len(cssMarkerEnd)])
}

// replaceCSSBlockContent swaps the existing marker-delimited block for block,
// leaving everything around it untouched. Marker presence alone is not
// "installed" — an older clauchy version's block content must be repaired,
// exactly like a stale exec or a missing return-type in the module object.
func replaceCSSBlockContent(src []byte, block string) []byte {
	si := bytes.Index(src, []byte(cssMarkerStart))
	ei := bytes.Index(src, []byte(cssMarkerEnd)) + len(cssMarkerEnd)
	var out bytes.Buffer
	out.Write(src[:si])
	out.WriteString(strings.TrimRight(block, "\n"))
	out.Write(src[ei:])
	return out.Bytes()
}

// appendCSSBlockContent appends the given CSS block content to src.
func appendCSSBlockContent(src []byte, block string) []byte {
	var out bytes.Buffer
	out.Write(src)
	if len(src) > 0 && src[len(src)-1] != '\n' {
		out.WriteByte('\n')
	}
	out.WriteString("\n")
	out.WriteString(block)
	return out.Bytes()
}

// ─── Backup ───────────────────────────────────────────────────────────────────

// nonClobberingBackup copies path to path.bak.<epoch>[.N], incrementing the
// numeric suffix until an unused name is found.
func nonClobberingBackup(path string) (string, error) {
	epoch := time.Now().Unix()
	base := fmt.Sprintf("%s.bak.%d", path, epoch)
	dest := base
	for n := 1; ; n++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = fmt.Sprintf("%s.%d", base, n)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("backup read %s: %w", path, err)
	}
	if err := os.WriteFile(dest, src, 0o644); err != nil {
		return "", fmt.Errorf("backup write %s: %w", dest, err)
	}
	return dest, nil
}

// ─── Run ──────────────────────────────────────────────────────────────────────

// Run idempotently installs the custom/clauchy Waybar module, CSS block, icon
// variants, and (optionally) Hyprland window rules.
//
// It evaluates conditions independently:
//
//	(a) custom/clauchy object present with exec containing "clauchy waybar"
//	(b) "custom/clauchy" listed in a modules-* array
//	(c) object carries return-type: json and interval: 60
//	(d) on-click state is accepted (valid on-click OR deliberately-absent on
//	    a terminal-less host)
//	(e) CSS marker block present in style.css
//	(f) Hyprland marker block present in hyprland.conf (when HyprlandConf set)
//
// Icons are always regenerated on every run (cheap, stateless, overwrite-safe).
// When all applicable conditions are satisfied this is a near-no-op (icons still
// regenerated; HyprChanged/ConfigChanged/CSSChanged remain false).
func Run(cfg RunConfig) (Result, error) {
	var res Result

	// ── Read config ──────────────────────────────────────────────────────────
	configSrc, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, ErrConfigNotFound
		}
		return Result{}, fmt.Errorf("reading config: %w", err)
	}

	// ── Tokenize ─────────────────────────────────────────────────────────────
	tokens, err := tokenizeJSONC(configSrc)
	if err != nil {
		return Result{}, fmt.Errorf("%w: tokenize: %v", ErrAmbiguousConfig, err)
	}

	// ── Scan ─────────────────────────────────────────────────────────────────
	sc, err := scanConfig(tokens, configSrc)
	if err != nil {
		return Result{}, err
	}
	if !sc.hasAnyModules() {
		return Result{}, ErrNoModulesArray
	}

	// ── Read CSS (non-fatal if missing) ───────────────────────────────────────
	cssSrc, cssErr := os.ReadFile(cfg.StylePath)
	if cssErr != nil && !os.IsNotExist(cssErr) {
		return Result{}, fmt.Errorf("reading style.css: %w", cssErr)
	}
	if err := validateCSSMarkers(cssSrc); err != nil {
		return Result{}, err
	}

	// ── Write icons (always — overwrite-safe regeneration) ────────────────────
	if cfg.DataHome != "" {
		iconDir := filepath.Join(cfg.DataHome, "clauchy")
		if err := writeIcons(iconDir); err != nil {
			res.Warnings = append(res.Warnings, "icon write failed: "+err.Error())
		} else {
			res.IconsWritten = true
		}
	}

	// ── Resolve terminal ──────────────────────────────────────────────────────
	terminal := resolveTerminal(sc.siblingOnClicks)
	onClickResolved := terminal != ""

	// ── Evaluate conditions ───────────────────────────────────────────────────
	condA := sc.clauchy.found && strings.Contains(sc.clauchy.exec, "clauchy waybar")
	condB := sc.clauchyInAnyArray()
	condC := sc.clauchy.hasRetType && sc.clauchy.hasInterval

	// condD: on-click is in an accepted state.
	// A present on-click must exactly match the canonical form built by
	// buildOnClickCmd for the terminal it names and the current Colorful flag —
	// any deviation (unknown terminal, old format lacking the font-size flag,
	// colorful mismatch, etc.) is treated as stale and triggers a repair,
	// mirroring the stale-exec repair path.
	// An absent on-click is accepted ONLY when no known terminal is resolvable.
	// When a terminal IS resolvable, an absent on-click is treated as a repair
	// trigger so the on-click gets written on the next run.
	var condD bool
	if sc.clauchy.found {
		if sc.clauchy.onClick != "" {
			t := terminalFromOnClick(sc.clauchy.onClick)
			condD = t != "" && sc.clauchy.onClick == buildOnClickCmd(t, cfg.Colorful)
		} else {
			// Absent on-click: accepted (no repair) only when no terminal resolves.
			// When a terminal is now available, the absent on-click is stale — repair it.
			condD = !onClickResolved
		}
	}

	// Compute icon dir for the CSS block (needed regardless of condE).
	iconDir := ""
	if cfg.DataHome != "" {
		iconDir = filepath.Join(cfg.DataHome, "clauchy")
	}

	// condE: the CSS block must exist AND carry the current content — an old
	// clauchy block (different icon paths, pre-icon glyph rules) is stale and
	// must be repaired, mirroring the stale-exec rule for the module object.
	condE := hasMarkerBlock(cssSrc) &&
		extractCSSBlock(cssSrc) == strings.TrimRight(buildCSSBlock(iconDir), "\n")

	// ── Hyprland ─────────────────────────────────────────────────────────────
	// Read hyprland.conf when a path is provided.
	// Missing file → warning + skip. Empty path → skip silently.
	if err := runHyprland(cfg, &res); err != nil {
		return Result{}, err
	}

	// ── Full no-op for Waybar config + CSS? ──────────────────────────────────
	if condA && condB && condC && condD && condE {
		// Set OnClickResolved based on the existing config state
		res.OnClickResolved = sc.clauchy.onClick != "" && terminalFromOnClick(sc.clauchy.onClick) != ""
		return res, nil
	}

	// ── Plan config edits ─────────────────────────────────────────────────────
	var configEdits []edit

	needsModuleRewrite := sc.clauchy.found && (!condA || !condC || !condD)
	needsModuleInsert := !sc.clauchy.found

	onClickCmd := ""
	if onClickResolved {
		onClickCmd = buildOnClickCmd(terminal, cfg.Colorful)
	}
	valueBlock := buildValueBlock(onClickCmd)
	memberText := `"custom/clauchy": ` + valueBlock

	if needsModuleRewrite {
		configEdits = append(configEdits, rewriteModuleEdit(sc.clauchy.keyStart, sc.clauchy.valueEnd, memberText))
	} else if needsModuleInsert {
		configEdits = append(configEdits, insertModuleEdit(configSrc, sc.rootClosePos, memberText))
	}

	if !condB {
		target := sc.targetModules()
		if target != nil {
			configEdits = append(configEdits, arrayAppendEdit(configSrc, target.arrayClose))
		}
	}

	// ── Apply config edits ────────────────────────────────────────────────────
	if len(configEdits) > 0 {
		bk, err := nonClobberingBackup(cfg.ConfigPath)
		if err != nil {
			return Result{}, fmt.Errorf("config backup: %w", err)
		}
		res.Backups = append(res.Backups, bk)

		newSrc := applyEdits(configSrc, configEdits)
		if err := os.WriteFile(cfg.ConfigPath, newSrc, 0o644); err != nil {
			return Result{}, fmt.Errorf("writing config: %w", err)
		}
		res.ConfigChanged = true
	}

	// ── CSS ───────────────────────────────────────────────────────────────────
	if !condE {
		if len(cssSrc) > 0 || cssErr == nil {
			bk, err := nonClobberingBackup(cfg.StylePath)
			if err != nil {
				return Result{}, fmt.Errorf("css backup: %w", err)
			}
			res.Backups = append(res.Backups, bk)
		}
		cssBlock := buildCSSBlock(iconDir)
		var newCSSSrc []byte
		if hasMarkerBlock(cssSrc) {
			newCSSSrc = replaceCSSBlockContent(cssSrc, cssBlock)
		} else {
			newCSSSrc = appendCSSBlockContent(cssSrc, cssBlock)
		}
		if err := os.WriteFile(cfg.StylePath, newCSSSrc, 0o644); err != nil {
			return Result{}, fmt.Errorf("writing css: %w", err)
		}
		res.CSSChanged = true
	}

	// ── Reload ────────────────────────────────────────────────────────────────
	if res.ConfigChanged || res.CSSChanged {
		if err := cfg.Reload(); err != nil {
			res.Warnings = append(res.Warnings, "reload failed: "+err.Error())
		}
	}

	// ── OnClickResolved ───────────────────────────────────────────────────────
	if needsModuleInsert || needsModuleRewrite {
		res.OnClickResolved = onClickResolved
		if !onClickResolved {
			res.Warnings = append(res.Warnings,
				"no known terminal (ghostty, alacritty, kitty, foot) found; module installed without on-click")
		}
	} else {
		res.OnClickResolved = sc.clauchy.onClick != "" && terminalFromOnClick(sc.clauchy.onClick) != ""
	}

	return res, nil
}

// runHyprland handles the Hyprland window rules surface.
// It is called from Run and mutates res in place.
// Returns non-nil only for fatal errors; a missing file is a warning, not fatal.
func runHyprland(cfg RunConfig, res *Result) error {
	if cfg.HyprlandConf == "" {
		return nil // non-Hyprland setup — skip silently
	}

	hyprSrc, err := os.ReadFile(cfg.HyprlandConf)
	if err != nil {
		if os.IsNotExist(err) {
			res.Warnings = append(res.Warnings,
				"hyprland.conf not found ("+cfg.HyprlandConf+"); skipping Hyprland window rules (non-Hyprland setup)")
			return nil
		}
		return fmt.Errorf("reading hyprland.conf: %w", err)
	}

	if hyprBlockUpToDate(hyprSrc) {
		return nil // already installed with current content — true no-op
	}

	// Backup then write (append or replace-in-place when block exists but stale).
	bk, err := nonClobberingBackup(cfg.HyprlandConf)
	if err != nil {
		return fmt.Errorf("hyprland backup: %w", err)
	}
	res.Backups = append(res.Backups, bk)

	var newSrc []byte
	if hasHyprBlock(hyprSrc) {
		// Stale block — replace in place so markers appear only once.
		newSrc = replaceHyprBlockContent(hyprSrc, hyprBlock())
	} else {
		// No block yet — append.
		newSrc = appendHyprBlock(hyprSrc)
	}
	if err := os.WriteFile(cfg.HyprlandConf, newSrc, 0o644); err != nil {
		return fmt.Errorf("writing hyprland.conf: %w", err)
	}
	res.HyprChanged = true

	// Reload via the provided Reloader (hyprctl reload injected by main; no-op in tests).
	if err := cfg.Reload(); err != nil {
		res.Warnings = append(res.Warnings, "hyprland reload failed: "+err.Error())
	}

	return nil
}
