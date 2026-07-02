package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RoninForge/akashi/internal/probe"
	"github.com/RoninForge/akashi/internal/registry"
)

// registryStub starts a registry that always answers with body, and returns
// a Client pointed at it.
func registryStub(t *testing.T, body string) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := registry.NewClient()
	c.BaseURL = srv.URL
	return c
}

// noNetworkEngine returns an Engine whose HTTP client fails the test if it
// is ever dialed, so a test that expects zero probe-time network calls (a
// resumed server, or a server with nothing keyless-probeable declared)
// catches a regression immediately instead of silently hitting the network.
func noNetworkEngine(t *testing.T) *probe.Engine {
	t.Helper()
	e := probe.NewEngine()
	e.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected network call to %s", r.URL)
		return nil, nil
	})}
	e.Now = func() time.Time { return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) }
	return e
}

func jsonResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// --- drain / limit ---

const fiveServers = `{
  "servers": [
    {"server": {"name":"census/one"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}},
    {"server": {"name":"census/two"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}},
    {"server": {"name":"census/three"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}},
    {"server": {"name":"census/four"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}},
    {"server": {"name":"census/five"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}}
  ],
  "metadata": {"nextCursor": ""}
}`

func TestRunDrainsAndLimits(t *testing.T) {
	client := registryStub(t, fiveServers)
	eng := noNetworkEngine(t) // none of these servers declare anything keyless-probeable
	outDir := t.TempDir()

	summary, err := Run(context.Background(), client, eng, Options{Out: outDir, Limit: 3, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Overall.Total != 3 {
		t.Errorf("total = %d, want 3 (the --limit)", summary.Overall.Total)
	}
	if summary.Overall.Counts[probe.Unknown] != 3 {
		t.Errorf("unknown count = %d, want 3", summary.Overall.Counts[probe.Unknown])
	}
	if summary.Limit != 3 || summary.Concurrency != 2 {
		t.Errorf("reproducibility params = %+v, want limit 3 concurrency 2", summary)
	}
	if summary.RegistryBaseURL != client.BaseURL {
		t.Errorf("registryBaseUrl = %q, want %q", summary.RegistryBaseURL, client.BaseURL)
	}

	lines := readLines(t, filepath.Join(outDir, RecordsFile))
	if len(lines) != 3 {
		t.Fatalf("records.jsonl has %d lines, want 3", len(lines))
	}
	for _, line := range lines {
		var r probe.Result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		if r.Verdict != probe.Unknown {
			t.Errorf("verdict = %q, want unknown", r.Verdict)
		}
		if r.CheckedAt == "" {
			t.Error("checkedAt missing from the record")
		}
	}

	sb, err := os.ReadFile(filepath.Join(outDir, SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Summary
	if err := json.Unmarshal(sb, &onDisk); err != nil {
		t.Fatalf("summary.json is not valid JSON: %v", err)
	}
	if onDisk.Overall.Total != 3 {
		t.Errorf("summary.json total = %d, want 3", onDisk.Overall.Total)
	}
}

func TestRunRequiresOut(t *testing.T) {
	if _, err := Run(context.Background(), nil, nil, Options{Out: "   "}); err == nil {
		t.Fatal("expected an error when --out is empty")
	}
}

// --- checkpoint resume ---

const resumeServers = `{
  "servers": [
    {"server": {"name":"resume/withrepo","repository":{"url":"https://github.com/acme/thing","source":"github"}}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}},
    {"server": {"name":"resume/plain"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}}
  ],
  "metadata": {"nextCursor": ""}
}`

func TestRunResumeSkipsAlreadyDone(t *testing.T) {
	outDir := t.TempDir()

	firstClient := registryStub(t, resumeServers)
	githubCalls := 0
	firstEng := probe.NewEngine()
	firstEng.Now = func() time.Time { return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) }
	firstEng.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" {
			t.Fatalf("unexpected network call to %s", r.URL)
		}
		githubCalls++
		return jsonResp(200, `{"archived":false,"pushed_at":"2026-06-25T00:00:00Z","stargazers_count":9,"license":{"spdx_id":"MIT"}}`), nil
	})}

	summary1, err := Run(context.Background(), firstClient, firstEng, Options{Out: outDir, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if summary1.Overall.Total != 2 {
		t.Fatalf("first run total = %d, want 2", summary1.Overall.Total)
	}
	if githubCalls != 1 {
		t.Fatalf("github calls = %d, want 1 (only resume/withrepo declares a repository)", githubCalls)
	}
	if summary1.Overall.Counts[probe.Healthy] != 1 || summary1.Overall.Counts[probe.Unknown] != 1 {
		t.Fatalf("first run counts = %+v, want one healthy one unknown", summary1.Overall.Counts)
	}
	firstLines := readLines(t, filepath.Join(outDir, RecordsFile))
	if len(firstLines) != 2 {
		t.Fatalf("records.jsonl has %d lines after the first run, want 2", len(firstLines))
	}

	// Rerun against the same --out. Both servers are already recorded, so a
	// resumed run must not probe either one again: the stub transport fails
	// the test if it is dialed at all.
	secondClient := registryStub(t, resumeServers)
	secondEng := noNetworkEngine(t)

	summary2, err := Run(context.Background(), secondClient, secondEng, Options{Out: outDir, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if summary2.Overall.Total != 2 {
		t.Errorf("resumed run total = %d, want 2", summary2.Overall.Total)
	}
	if summary2.Overall.Counts[probe.Healthy] != 1 || summary2.Overall.Counts[probe.Unknown] != 1 {
		t.Errorf("resumed run counts = %+v, want the same as the first run", summary2.Overall.Counts)
	}
	secondLines := readLines(t, filepath.Join(outDir, RecordsFile))
	if len(secondLines) != 2 {
		t.Errorf("records.jsonl grew to %d lines after resume, want still 2 (no duplicate records)", len(secondLines))
	}
}

// --- name validation surfaced in the summary ---

const nameIssueServers = `{
  "servers": [
    {"server": {"name":"io.github.acme/thing"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}},
    {"server": {"name":"io.github.acme/Thing"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}},
    {"server": {"name":"acme/nodot"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}}
  ],
  "metadata": {"nextCursor": ""}
}`

func TestRunSurfacesNameIssuesInSummaryAndWarns(t *testing.T) {
	client := registryStub(t, nameIssueServers)
	eng := noNetworkEngine(t) // none of these servers declare anything keyless-probeable
	outDir := t.TempDir()
	var stderr bytes.Buffer

	summary, err := Run(context.Background(), client, eng, Options{Out: outDir, Concurrency: 2, Progress: &stderr})
	if err != nil {
		t.Fatal(err)
	}

	// io.github.acme/Thing is the only bad-charset name (the uppercase T).
	if summary.NameIssues.BadCharset.Count != 1 {
		t.Errorf("badCharset count = %d, want 1", summary.NameIssues.BadCharset.Count)
	}
	// acme/nodot has no "." in its namespace segment.
	if summary.NameIssues.BadShape.Count != 1 {
		t.Errorf("badShape count = %d, want 1", summary.NameIssues.BadShape.Count)
	}
	// io.github.acme/thing and io.github.acme/Thing collide once lowercased.
	if summary.NameIssues.CaseCollisions.Count != 2 {
		t.Errorf("caseCollisions count = %d, want 2", summary.NameIssues.CaseCollisions.Count)
	}
	if !strings.Contains(stderr.String(), "warning: name validation found") {
		t.Errorf("stderr missing the name-validation warning line, got %q", stderr.String())
	}

	// The scan must still complete and record every server: name issues are
	// non-fatal.
	if summary.Overall.Total != 3 {
		t.Errorf("total = %d, want 3 (a bad name must not drop a server)", summary.Overall.Total)
	}

	sb, err := os.ReadFile(filepath.Join(outDir, SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Summary
	if err := json.Unmarshal(sb, &onDisk); err != nil {
		t.Fatalf("summary.json is not valid JSON: %v", err)
	}
	if onDisk.NameIssues.BadCharset.Count != 1 || onDisk.NameIssues.BadShape.Count != 1 || onDisk.NameIssues.CaseCollisions.Count != 2 {
		t.Errorf("summary.json nameIssues = %+v, want badCharset 1 / badShape 1 / caseCollisions 2", onDisk.NameIssues)
	}
}

const cleanNameServers = `{
  "servers": [
    {"server": {"name":"io.github.acme/one"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}},
    {"server": {"name":"io.github.acme/two"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}}
  ],
  "metadata": {"nextCursor": ""}
}`

func TestRunNoWarningWhenNamesAreClean(t *testing.T) {
	client := registryStub(t, cleanNameServers)
	eng := noNetworkEngine(t)
	outDir := t.TempDir()
	var stderr bytes.Buffer

	summary, err := Run(context.Background(), client, eng, Options{Out: outDir, Concurrency: 2, Progress: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if summary.NameIssues.hasIssues() {
		t.Errorf("expected no name issues for these names, got %+v", summary.NameIssues)
	}
	if strings.Contains(stderr.String(), "warning: name validation") {
		t.Errorf("did not expect a name-validation warning for these names, got %q", stderr.String())
	}
}

// --- summary rate math ---

func TestSummarizeRateMath(t *testing.T) {
	results := []probe.Result{
		{Verdict: probe.Healthy},
		{Verdict: probe.Healthy},
		{Verdict: probe.Degraded},
		{Verdict: probe.Dead},
	}
	b := summarize(results)
	if b.Total != 4 {
		t.Fatalf("total = %d, want 4", b.Total)
	}
	wantCounts := map[probe.Verdict]int{probe.Healthy: 2, probe.Degraded: 1, probe.Dead: 1, probe.Unknown: 0}
	for v, want := range wantCounts {
		if got := b.Counts[v]; got != want {
			t.Errorf("counts[%s] = %d, want %d", v, got, want)
		}
	}
	wantRates := map[probe.Verdict]float64{probe.Healthy: 0.5, probe.Degraded: 0.25, probe.Dead: 0.25, probe.Unknown: 0}
	for v, want := range wantRates {
		if got := b.Rates[v]; got != want {
			t.Errorf("rates[%s] = %v, want %v", v, got, want)
		}
	}
}

func TestSummarizeEmptyPopulationHasZeroRatesNotNaN(t *testing.T) {
	b := summarize(nil)
	if b.Total != 0 {
		t.Fatalf("total = %d, want 0", b.Total)
	}
	for _, v := range allVerdicts {
		if b.Rates[v] != 0 {
			t.Errorf("rates[%s] = %v, want 0 for an empty population", v, b.Rates[v])
		}
	}
}

func TestRemoteBearingFiltersToRemoteDeclaringServers(t *testing.T) {
	results := []probe.Result{
		{Name: "has-remote", Signals: probe.Signals{Remotes: []probe.RemoteSignal{{URL: "https://mcp.example.com"}}}},
		{Name: "no-remote"},
	}
	got := remoteBearing(results)
	if len(got) != 1 || got[0].Name != "has-remote" {
		t.Errorf("remoteBearing = %+v, want only %q", got, "has-remote")
	}
}
