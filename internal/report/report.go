// Package report renders a probe.Result as a human-readable summary, as JSON
// (pretty-printed or one compact line for a JSONL dataset), or as a
// shields.io endpoint badge.
package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/RoninForge/akashi/internal/probe"
)

// ANSI colors, applied only when color is requested.
const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiGray   = "\033[90m"
	ansiBold   = "\033[1m"
)

// WritePretty prints a check table and an overall verdict line.
func WritePretty(w io.Writer, r probe.Result, color bool) {
	c := colorizer(color)

	fmt.Fprintf(w, "%s  %s\n", c(ansiBold, r.Name), c(ansiGray, "checked "+r.CheckedAt+" UTC, keyless"))
	fmt.Fprintln(w)

	for _, ch := range r.Checks {
		sym, col := statusGlyph(ch.Status)
		line := fmt.Sprintf("  %s  %-30s", c(col, sym), ch.Name)
		if ch.Detail != "" {
			line += "  " + c(ansiGray, ch.Detail)
		}
		fmt.Fprintln(w, line)
	}

	fmt.Fprintln(w)
	verdictSym, verdictCol := verdictGlyph(r.Verdict)
	fmt.Fprintf(w, "  %s  %s\n", c(verdictCol, verdictSym), c(ansiBold, verdictLine(r)))
	if len(r.Reasons) > 0 && r.Verdict != probe.Healthy {
		fmt.Fprintf(w, "     %s\n", c(ansiGray, "reasons: "+joinReasons(r.Reasons)))
	}
}

// WriteJSON writes the full Result as pretty-printed JSON.
func WriteJSON(w io.Writer, r probe.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// WriteJSONLine writes the full Result as compact, single-line JSON with a
// trailing newline: one row of a JSONL dataset. It uses the same encoder
// settings as WriteJSON (no HTML escaping), so a census record carries
// exactly the same fields and values `check <server> --json` prints, just
// without the pretty indentation a multi-line report needs and a JSONL
// record cannot have.
func WriteJSONLine(w io.Writer, r probe.Result) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

func verdictLine(r probe.Result) string {
	switch r.Verdict {
	case probe.Healthy:
		return "healthy"
	case probe.Degraded:
		return "degraded (usable, but something is broken)"
	case probe.Dead:
		return "dead (nothing works)"
	case probe.Unknown:
		return "unknown (nothing keyless-probeable was declared)"
	default:
		return string(r.Verdict)
	}
}

func statusGlyph(s probe.Status) (string, string) {
	switch s {
	case probe.Pass:
		return "PASS", ansiGreen
	case probe.Warn:
		return "WARN", ansiYellow
	case probe.Fail:
		return "FAIL", ansiRed
	default:
		return "SKIP", ansiGray
	}
}

func verdictGlyph(v probe.Verdict) (string, string) {
	switch v {
	case probe.Healthy:
		return "OK  ", ansiGreen
	case probe.Degraded:
		return "WARN", ansiYellow
	case probe.Dead:
		return "DEAD", ansiRed
	default:
		return "????", ansiGray
	}
}

func joinReasons(rs []string) string {
	out := ""
	for i, r := range rs {
		if i > 0 {
			out += ", "
		}
		out += r
	}
	return out
}

func colorizer(enabled bool) func(code, s string) string {
	if !enabled {
		return func(_, s string) string { return s }
	}
	return func(code, s string) string { return code + s + ansiReset }
}
