package report

import (
	"encoding/json"
	"io"

	"github.com/RoninForge/akashi/internal/probe"
)

// Badge is the shields.io endpoint schema (schemaVersion 1). Extra underscore
// fields are ignored by shields.io and carried for our own embeds.
//
// Embed: img.shields.io/endpoint?url=<hosted badge json url>
type Badge struct {
	SchemaVersion int    `json:"schemaVersion"`
	Label         string `json:"label"`
	Message       string `json:"message"`
	Color         string `json:"color"`
	CacheSeconds  int    `json:"cacheSeconds"`

	Server    string `json:"_server,omitempty"`
	CheckedAt string `json:"_checkedAt,omitempty"`
	Verdict   string `json:"_verdict,omitempty"`
}

// NewBadge builds a shields.io endpoint badge from a probe result. A healthy
// server reads "verified <date>"; anything else reads its verdict, so a stale
// or broken server can never masquerade as verified.
func NewBadge(r probe.Result) Badge {
	message := "verified " + r.CheckedAt
	color := "brightgreen"
	switch r.Verdict {
	case probe.Healthy:
		// message/color already set
	case probe.Degraded:
		message = "degraded"
		color = "yellow"
	case probe.Dead:
		message = "broken"
		color = "red"
	default:
		message = "unknown"
		color = "lightgrey"
	}
	return Badge{
		SchemaVersion: 1,
		Label:         "MCP health",
		Message:       message,
		Color:         color,
		CacheSeconds:  86400,
		Server:        r.Name,
		CheckedAt:     r.CheckedAt,
		Verdict:       string(r.Verdict),
	}
}

// WriteBadge writes the shields.io endpoint JSON.
func WriteBadge(w io.Writer, r probe.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(NewBadge(r))
}
