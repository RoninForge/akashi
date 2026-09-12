package scan

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RoninForge/akashi/internal/probe"
	"github.com/RoninForge/akashi/internal/registry"
	"github.com/RoninForge/akashi/internal/report"
)

// ReprobeFile is the JSONL filename for second-pass readings written inside
// --out. It is a SEPARATE file on purpose: records.jsonl is the census as
// observed and is never edited after the fact, so a re-probe is additional
// evidence recorded alongside it, never a correction applied to it.
const ReprobeFile = "reprobe.jsonl"

// Re-probe defaults. A census long enough for targets to change state records
// a transient outage as a property of the thing measured; the 2026-09-09
// census ran 16h59m and one operator's endpoints flipped 1,277 stateless-true
// readings to 270 while its health verdicts barely moved. Nothing in the data
// said so. This pass is what says so.
const (
	// DefaultReprobeThreshold is how far a namespace's signal rate must move
	// between editions, in percentage points, to earn a second reading.
	DefaultReprobeThreshold = 10.0
	// DefaultReprobeMinServers keeps small namespaces out: a 3-server
	// namespace swings 33 points when one server blinks, which is noise.
	DefaultReprobeMinServers = 25
	// DefaultReprobeMaxServers bounds the second pass's wall time. A census
	// that cannot finish is worse than one with an undisclosed outage.
	DefaultReprobeMaxServers = 3000
)

// reprobeSignals are the readiness booleans a flaky endpoint flips wholesale
// while the health verdict holds. Health is tracked too, as the control: a
// namespace whose verdicts moved WITH its signals probably really changed.
var reprobeSignals = []string{"statelessAccepted", "discoverSupported", "sessionIssued", "sessionRequired", "healthy"}

// nsStats accumulates one namespace's signal counts for one edition.
type nsStats struct {
	servers          int
	readinessBearing int
	counts           map[string]int
}

func newNSStats() *nsStats { return &nsStats{counts: map[string]int{}} }

// rate returns the share of the population the signal is measured over: the
// readiness-bearing servers for a readiness signal, every server for health.
// The bool reports whether the rate is measurable at all.
func (s *nsStats) rate(signal string) (float64, bool) {
	denom := s.readinessBearing
	if signal == "healthy" {
		denom = s.servers
	}
	if denom == 0 {
		return 0, false
	}
	return float64(s.counts[signal]) / float64(denom) * 100, true
}

// NamespaceDrift is one namespace whose aggregate signals moved enough
// between editions to earn a second reading, plus what that reading found.
type NamespaceDrift struct {
	Namespace   string  `json:"namespace"`
	Servers     int     `json:"servers"`
	Signal      string  `json:"signal"` // the signal that moved furthest
	PreviousPct float64 `json:"previousPct"`
	CurrentPct  float64 `json:"currentPct"`
	DeltaPoints float64 `json:"deltaPoints"`

	// The second reading. Reprobed is how many were actually re-probed;
	// Agreed/Changed count those whose signal read the same or differently the
	// second time, and NoAnswer those the re-probe could not reach at all.
	// NoAnswer is absence of evidence and is never counted as either.
	Reprobed int `json:"reprobed"`
	Agreed   int `json:"agreed"`
	Changed  int `json:"changed"`
	NoAnswer int `json:"noAnswer"`
}

// ReprobeReport records the whole second pass. It is written into
// summary.json so a reader who never opens reprobe.jsonl still learns that a
// rate in this census has a contested namespace inside it.
type ReprobeReport struct {
	ComparedTo      string  `json:"comparedTo"`
	ThresholdPoints float64 `json:"thresholdPoints"`
	MinServers      int     `json:"minServers"`
	MaxServers      int     `json:"maxServers"`
	StartedAt       string  `json:"startedAt"`
	FinishedAt      string  `json:"finishedAt"`
	Candidates      int     `json:"candidates"`
	// Truncated reports that MaxServers stopped the pass before every
	// candidate namespace was re-probed. Recorded rather than silently
	// dropped: a partial second pass that reads as a complete one would be a
	// false all-clear on exactly the question this pass exists to answer.
	Truncated  bool             `json:"truncated"`
	Namespaces []NamespaceDrift `json:"namespaces"`
}

// namespaceOf splits "namespace/leaf" at the first slash. Names without one
// are their own namespace, which validateNames already flags separately.
func namespaceOf(name string) string {
	if i := strings.Index(name, "/"); i >= 0 {
		return name[:i]
	}
	return name
}

// statsByNamespace accumulates per-namespace signal counts over one edition's
// results.
func statsByNamespace(results []probe.Result) map[string]*nsStats {
	out := map[string]*nsStats{}
	for _, r := range results {
		ns := namespaceOf(r.Name)
		s, ok := out[ns]
		if !ok {
			s = newNSStats()
			out[ns] = s
		}
		s.servers++
		if r.Verdict == probe.Healthy {
			s.counts["healthy"]++
		}
		if r.Readiness == nil {
			continue
		}
		s.readinessBearing++
		if r.Readiness.StatelessAccepted {
			s.counts["statelessAccepted"]++
		}
		if r.Readiness.DiscoverSupported {
			s.counts["discoverSupported"]++
		}
		if r.Readiness.SessionIssued {
			s.counts["sessionIssued"]++
		}
		if r.Readiness.SessionRequired {
			s.counts["sessionRequired"]++
		}
	}
	return out
}

// signalOf reads one tracked signal off a result, reporting false when the
// result cannot speak to it (no readiness pass ran).
func signalOf(r probe.Result, signal string) (bool, bool) {
	if signal == "healthy" {
		return r.Verdict == probe.Healthy, true
	}
	if r.Readiness == nil {
		return false, false
	}
	switch signal {
	case "statelessAccepted":
		return r.Readiness.StatelessAccepted, true
	case "discoverSupported":
		return r.Readiness.DiscoverSupported, true
	case "sessionIssued":
		return r.Readiness.SessionIssued, true
	case "sessionRequired":
		return r.Readiness.SessionRequired, true
	}
	return false, false
}

// driftCandidates finds namespaces whose signal rates moved at least
// thresholdPoints between the previous edition and this one, largest move
// first so a MaxServers cap spends its budget on the worst offenders.
func driftCandidates(prev, cur map[string]*nsStats, thresholdPoints float64, minServers int) []NamespaceDrift {
	var out []NamespaceDrift
	for ns, c := range cur {
		p, ok := prev[ns]
		if !ok {
			continue // new namespace: nothing to compare against, not drift
		}
		var best NamespaceDrift
		for _, sig := range reprobeSignals {
			denomCur, denomPrev := c.readinessBearing, p.readinessBearing
			if sig == "healthy" {
				denomCur, denomPrev = c.servers, p.servers
			}
			if denomCur < minServers || denomPrev < minServers {
				continue
			}
			cr, okc := c.rate(sig)
			pr, okp := p.rate(sig)
			if !okc || !okp {
				continue
			}
			d := math.Abs(cr - pr)
			if d >= thresholdPoints && d > best.DeltaPoints {
				best = NamespaceDrift{
					Namespace:   ns,
					Servers:     c.servers,
					Signal:      sig,
					PreviousPct: math.Round(pr*10) / 10,
					CurrentPct:  math.Round(cr*10) / 10,
					DeltaPoints: math.Round(d*10) / 10,
				}
			}
		}
		if best.Signal != "" {
			out = append(out, best)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeltaPoints != out[j].DeltaPoints {
			return out[i].DeltaPoints > out[j].DeltaPoints
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out
}

// runReprobe re-probes the servers of every drifting namespace, writes each
// second reading to ReprobeFile, and reports what the second reading found.
// It never mutates results or recordsPath.
func runReprobe(
	ctx context.Context,
	eng *probe.Engine,
	opts Options,
	servers []registry.Server,
	results []probe.Result,
	prevResults []probe.Result,
	progress io.Writer,
) (*ReprobeReport, error) {
	threshold, minServers, maxServers := opts.ReprobeThreshold, opts.ReprobeMinServers, opts.ReprobeMaxServers
	if threshold <= 0 {
		threshold = DefaultReprobeThreshold
	}
	if minServers <= 0 {
		minServers = DefaultReprobeMinServers
	}
	if maxServers <= 0 {
		maxServers = DefaultReprobeMaxServers
	}

	candidates := driftCandidates(statsByNamespace(prevResults), statsByNamespace(results), threshold, minServers)
	rep := &ReprobeReport{
		ComparedTo:      opts.Compare,
		ThresholdPoints: threshold,
		MinServers:      minServers,
		MaxServers:      maxServers,
		StartedAt:       time.Now().UTC().Format(time.RFC3339),
		Candidates:      len(candidates),
	}
	if len(candidates) == 0 {
		rep.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		fmt.Fprintf(progress, "re-probe: no namespace moved %.0f+ points against %s\n", threshold, opts.Compare)
		return rep, nil
	}

	byName := make(map[string]registry.Server, len(servers))
	for _, s := range servers {
		byName[s.Name] = s
	}
	firstReading := make(map[string]probe.Result, len(results))
	for _, r := range results {
		firstReading[r.Name] = r
	}

	path := filepath.Join(opts.Out, ReprobeFile)
	// #nosec G304 -- path is derived from --out, an operator-supplied CLI flag.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("scan: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	budget := maxServers
	for i := range candidates {
		d := &candidates[i]
		var targets []registry.Server
		for _, r := range results {
			if namespaceOf(r.Name) != d.Namespace {
				continue
			}
			if s, ok := byName[r.Name]; ok {
				targets = append(targets, s)
			}
		}
		if len(targets) > budget {
			rep.Truncated = true
			targets = targets[:budget]
		}
		if len(targets) == 0 {
			rep.Namespaces = append(rep.Namespaces, *d)
			continue
		}
		budget -= len(targets)

		fmt.Fprintf(progress, "re-probe: %s moved %.1f points on %s, re-probing %d server(s)\n",
			d.Namespace, d.DeltaPoints, d.Signal, len(targets))

		if err := probeAll(ctx, eng, targets, opts.Concurrency, opts.Timeout, func(r probe.Result) error {
			d.Reprobed++
			was, hadFirst := signalOf(firstReading[r.Name], d.Signal)
			now, answered := signalOf(r, d.Signal)
			switch {
			case !answered:
				// The re-probe could not speak to the signal. Our own failure is
				// never an observation about the target.
				d.NoAnswer++
			case hadFirst && was == now:
				d.Agreed++
			case hadFirst:
				d.Changed++
			}
			return report.WriteJSONLine(f, r)
		}); err != nil {
			return nil, err
		}
		if err := f.Sync(); err != nil {
			return nil, fmt.Errorf("scan: sync %s: %w", path, err)
		}
		rep.Namespaces = append(rep.Namespaces, *d)
		if budget <= 0 {
			if i < len(candidates)-1 {
				rep.Truncated = true
			}
			break
		}
	}
	rep.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	return rep, nil
}

// loadEdition reads a previous census's records, accepting either the census
// directory or its records.jsonl directly, so --compare takes whichever path
// an operator has to hand.
func loadEdition(path string) ([]probe.Result, error) {
	p := path
	// #nosec G703 -- path is --compare, an operator-supplied CLI flag naming a
	// previous census to read.
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		p = filepath.Join(p, RecordsFile)
	}
	byName, err := loadCheckpoint(p)
	if err != nil {
		return nil, err
	}
	if len(byName) == 0 {
		return nil, fmt.Errorf("no usable records in %s", p)
	}
	out := make([]probe.Result, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	return out, nil
}
