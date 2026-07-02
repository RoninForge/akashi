// Package scan drains the whole official MCP registry and runs the same
// keyless probe.Engine that "akashi check" uses against every server, so a
// bulk census and a single check are always the same measurement over the
// same struct. It only adds what a census needs on top of that: bounded
// concurrency, a checkpointed JSONL so an interrupted run resumes without
// re-probing, and an aggregate summary. It never reimplements a probe or
// changes a verdict.
package scan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RoninForge/akashi/internal/probe"
	"github.com/RoninForge/akashi/internal/registry"
	"github.com/RoninForge/akashi/internal/report"
	"github.com/RoninForge/akashi/internal/version"
)

// RecordsFile is the JSONL filename written inside --out: one probe.Result
// per line, the exact fields "akashi check <server> --json" prints.
const RecordsFile = "records.jsonl"

// SummaryFile is the aggregate-report filename written inside --out.
const SummaryFile = "summary.json"

// DefaultConcurrency is a modest worker pool size: polite to the registry,
// package indexes, and remote endpoints a census touches, while still
// finishing a full drain of the registry in a practical amount of time.
const DefaultConcurrency = 8

// DefaultTimeout bounds one server's full probe set. It mirrors the default
// "akashi check" uses for the same overall budget.
const DefaultTimeout = 60 * time.Second

// Options configures one census run.
type Options struct {
	// Out is the output directory. Required. RecordsFile and SummaryFile are
	// written inside it.
	Out string
	// Limit caps how many servers are drained from the registry. 0 means the
	// whole registry.
	Limit int
	// Concurrency is the worker pool size. <= 0 falls back to
	// DefaultConcurrency.
	Concurrency int
	// Timeout bounds one server's full probe set. <= 0 falls back to
	// DefaultTimeout.
	Timeout time.Duration
	// Progress receives one line per completed server ("1234/13886 name
	// verdict"), plus a startup line noting how many were already recorded.
	// A nil Progress disables progress reporting.
	Progress io.Writer
}

// Breakdown is a verdict census over some population: the whole run, or one
// segment of it (see Summary.Segments). Rates are Counts[v] / Total; Total
// is the denominator.
type Breakdown struct {
	Total  int                       `json:"total"`
	Counts map[probe.Verdict]int     `json:"counts"`
	Rates  map[probe.Verdict]float64 `json:"rates"`
}

// Summary is the aggregate report written to SummaryFile alongside
// RecordsFile. It carries enough reproducibility parameters that a "State of
// MCP" index built from it can cite exactly how the numbers were produced.
type Summary struct {
	RegistryBaseURL string `json:"registryBaseUrl"`
	AkashiVersion   string `json:"akashiVersion"`
	Concurrency     int    `json:"concurrency"`
	Limit           int    `json:"limit"` // 0 means the whole registry
	StartedAt       string `json:"startedAt"`
	FinishedAt      string `json:"finishedAt"`

	// Overall is the verdict census across every server this run targeted.
	Overall Breakdown `json:"overall"`
	// Segments key the same kind of census by a defining trait. "remote"
	// (servers that declare at least one hosted remote endpoint) is the
	// headline: a reachable, conformant remote is the strongest keyless
	// liveness proof a census can gather, since its domain is verified simply
	// by being probed live over HTTPS.
	Segments map[string]Breakdown `json:"segments"`
	// NameIssues flags registry names that would not survive the planned
	// <namespace>/<name> per-server page routing (see validateNames). It is
	// a heads-up for the index build, not a scan failure: every server is
	// still probed and recorded regardless of what this reports.
	NameIssues NameIssues `json:"nameIssues"`
}

// allVerdicts lists every possible probe.Verdict so a Breakdown always has a
// zero entry for a verdict that never occurred, rather than an absent key.
var allVerdicts = []probe.Verdict{probe.Healthy, probe.Degraded, probe.Dead, probe.Unknown}

// Run drains the registry (respecting opts.Limit), probes every server that
// is not already recorded in an existing RecordsFile under opts.Out, appends
// each freshly probed result as it completes, and writes SummaryFile once
// the whole targeted population has a record.
//
// client and eng are pre-configured by the caller (base URL, GitHub token,
// HTTP transport); Run only orchestrates draining, concurrency, and
// checkpointing around them. eng is shared across the whole worker pool:
// its HTTP client and schema cache are safe for concurrent use.
func Run(ctx context.Context, client *registry.Client, eng *probe.Engine, opts Options) (Summary, error) {
	if strings.TrimSpace(opts.Out) == "" {
		return Summary{}, fmt.Errorf("scan: --out is required")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	progress := opts.Progress
	if progress == nil {
		progress = io.Discard
	}

	if err := os.MkdirAll(opts.Out, 0o700); err != nil {
		return Summary{}, fmt.Errorf("scan: create out dir: %w", err)
	}
	recordsPath := filepath.Join(opts.Out, RecordsFile)
	summaryPath := filepath.Join(opts.Out, SummaryFile)

	started := time.Now().UTC()

	servers, err := client.Drain(ctx, opts.Limit)
	if err != nil {
		return Summary{}, fmt.Errorf("scan: drain registry: %w", err)
	}

	// Name validation is a pre-flight check over the drained population, not
	// a probe: it never touches the network and never drops or reorders a
	// server, so it runs once up front regardless of --limit or resume state.
	nameIssues := validateNames(servers)
	if nameIssues.hasIssues() {
		fmt.Fprintf(progress, "warning: name validation found %d bad-charset, %d bad-shape, %d case-collision name(s); see summary.json nameIssues\n",
			nameIssues.BadCharset.Count, nameIssues.BadShape.Count, nameIssues.CaseCollisions.Count)
	}

	done, err := loadCheckpoint(recordsPath)
	if err != nil {
		return Summary{}, fmt.Errorf("scan: read checkpoint: %w", err)
	}

	results := make([]probe.Result, 0, len(servers))
	pending := make([]registry.Server, 0, len(servers))
	for _, s := range servers {
		if r, ok := done[s.Name]; ok {
			results = append(results, r)
			continue
		}
		pending = append(pending, s)
	}

	total := len(servers)
	completed := total - len(pending)
	fmt.Fprintf(progress, "%d/%d already recorded, probing %d\n", completed, total, len(pending))

	// #nosec G304 -- recordsPath is derived from --out, an operator-supplied
	// CLI flag naming where to write the dataset, exactly like any
	// file-writing CLI tool.
	f, err := os.OpenFile(recordsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Summary{}, fmt.Errorf("scan: open %s: %w", recordsPath, err)
	}
	defer func() { _ = f.Close() }()

	if err := probeAll(ctx, eng, pending, opts.Concurrency, opts.Timeout, func(r probe.Result) error {
		completed++
		fmt.Fprintf(progress, "%d/%d %s %s\n", completed, total, r.Name, r.Verdict)
		results = append(results, r)
		if err := report.WriteJSONLine(f, r); err != nil {
			return fmt.Errorf("scan: write record for %s: %w", r.Name, err)
		}
		return f.Sync()
	}); err != nil {
		return Summary{}, err
	}
	if err := ctx.Err(); err != nil {
		// Interrupted: everything completed so far is already flushed to
		// recordsPath, so rerunning with the same --out resumes from here. No
		// summary is written for a population that was never fully probed.
		return Summary{}, err
	}

	finished := time.Now().UTC()
	summary := Summary{
		RegistryBaseURL: client.BaseURL,
		AkashiVersion:   version.Get().Version,
		Concurrency:     opts.Concurrency,
		Limit:           opts.Limit,
		StartedAt:       started.Format(time.RFC3339),
		FinishedAt:      finished.Format(time.RFC3339),
		Overall:         summarize(results),
		Segments: map[string]Breakdown{
			"remote": summarize(remoteBearing(results)),
		},
		NameIssues: nameIssues,
	}

	if err := writeSummary(summaryPath, summary); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

// probeAll runs eng.ProbeServer over servers with a bounded worker pool and
// hands each completed Result to onResult, in the order servers complete
// (not the order they were queued). onResult also owns the (single-writer)
// side effect of appending the record, so probeAll never touches the output
// file directly.
func probeAll(ctx context.Context, eng *probe.Engine, servers []registry.Server, concurrency int, timeout time.Duration, onResult func(probe.Result) error) error {
	if len(servers) == 0 {
		return nil
	}

	jobs := make(chan registry.Server)
	resultsCh := make(chan probe.Result)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				rctx, cancel := context.WithTimeout(ctx, timeout)
				r := eng.ProbeServer(rctx, s)
				cancel()
				select {
				case resultsCh <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, s := range servers {
			select {
			case jobs <- s:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var onResultErr error
	for r := range resultsCh {
		if onResultErr != nil {
			continue // drain resultsCh so workers never block on a full channel
		}
		onResultErr = onResult(r)
	}
	return onResultErr
}

// loadCheckpoint reads an existing RecordsFile (if any) and returns the
// results already recorded, keyed by server name, so Run can skip
// re-probing them. A line that fails to parse (for example a partial write
// left by a hard kill mid-record) is skipped rather than treated as fatal:
// it is simply not counted as done, so a later run re-probes that one server
// and appends a fresh record after it. The file itself is never rewritten,
// only appended to, which is what keeps resume crash-safe.
func loadCheckpoint(path string) (map[string]probe.Result, error) {
	// #nosec G304 -- path is derived from --out, an operator-supplied CLI flag.
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]probe.Result{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	done := make(map[string]probe.Result)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r probe.Result
		if err := json.Unmarshal(line, &r); err != nil || r.Name == "" {
			continue
		}
		done[r.Name] = r
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return done, nil
}

// writeSummary writes summary as pretty-printed JSON to path.
func writeSummary(path string, summary Summary) error {
	// #nosec G304 -- path is derived from --out, an operator-supplied CLI flag.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("scan: create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(summary); err != nil {
		return fmt.Errorf("scan: write %s: %w", path, err)
	}
	return nil
}

// remoteBearing returns the subset of results for servers that declared at
// least one remote endpoint (see Summary.Segments).
func remoteBearing(results []probe.Result) []probe.Result {
	out := make([]probe.Result, 0, len(results))
	for _, r := range results {
		if len(r.Signals.Remotes) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// summarize computes a verdict Breakdown over results. Total is len(results)
// regardless of which verdicts occur, so every Breakdown carries a stable
// denominator even when a verdict's count is zero.
func summarize(results []probe.Result) Breakdown {
	b := Breakdown{
		Total:  len(results),
		Counts: make(map[probe.Verdict]int, len(allVerdicts)),
		Rates:  make(map[probe.Verdict]float64, len(allVerdicts)),
	}
	for _, v := range allVerdicts {
		b.Counts[v] = 0
	}
	for _, r := range results {
		b.Counts[r.Verdict]++
	}
	for _, v := range allVerdicts {
		if b.Total == 0 {
			b.Rates[v] = 0
			continue
		}
		b.Rates[v] = float64(b.Counts[v]) / float64(b.Total)
	}
	return b
}
