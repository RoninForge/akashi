package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RoninForge/akashi/internal/probe"
)

func TestNewBadgeHealthy(t *testing.T) {
	b := NewBadge(probe.Result{Name: "x", Verdict: probe.Healthy, CheckedAt: "2026-07-01"})
	if b.Message != "verified 2026-07-01" {
		t.Errorf("message = %q, want verified 2026-07-01", b.Message)
	}
	if b.Color != "brightgreen" {
		t.Errorf("color = %q, want brightgreen", b.Color)
	}
	if b.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", b.SchemaVersion)
	}
}

func TestNewBadgeBrokenNeverReadsVerified(t *testing.T) {
	for _, v := range []probe.Verdict{probe.Degraded, probe.Dead, probe.Unknown} {
		b := NewBadge(probe.Result{Verdict: v, CheckedAt: "2026-07-01"})
		if strings.Contains(b.Message, "verified") {
			t.Errorf("verdict %q produced a 'verified' badge: %q", v, b.Message)
		}
		if b.Color == "brightgreen" {
			t.Errorf("verdict %q produced a green badge", v)
		}
	}
}

func TestWriteBadgeIsShieldsShaped(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteBadge(&buf, probe.Result{Verdict: probe.Dead, CheckedAt: "2026-07-01", Name: "n"}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("badge is not valid JSON: %v", err)
	}
	for _, k := range []string{"schemaVersion", "label", "message", "color"} {
		if _, ok := m[k]; !ok {
			t.Errorf("badge missing required shields field %q", k)
		}
	}
}

func TestWritePrettyShowsVerdictAndReasons(t *testing.T) {
	var buf bytes.Buffer
	r := probe.Result{
		Name:      "io.github.acme/thing",
		Verdict:   probe.Degraded,
		CheckedAt: "2026-07-01",
		Reasons:   []string{"repo_404"},
		Checks:    []probe.Check{{Name: "repo reachable", Status: probe.Fail, Detail: "404"}},
	}
	WritePretty(&buf, r, false)
	out := buf.String()
	for _, want := range []string{"io.github.acme/thing", "repo reachable", "degraded", "repo_404"} {
		if !strings.Contains(out, want) {
			t.Errorf("pretty output missing %q\n---\n%s", want, out)
		}
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	in := probe.Result{Name: "n", Verdict: probe.Healthy, CheckedAt: "2026-07-01"}
	if err := WriteJSON(&buf, in); err != nil {
		t.Fatal(err)
	}
	var out probe.Result
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.Verdict != in.Verdict {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}
