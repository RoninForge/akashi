package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RoninForge/akashi/internal/probe"
)

// res builds one result with a readiness pass carrying the given stateless
// reading. verdict is the health verdict.
func res(name string, verdict probe.Verdict, stateless bool) probe.Result {
	return probe.Result{
		Name:      name,
		Verdict:   verdict,
		Readiness: &probe.ReadinessSignal{StatelessAccepted: stateless},
	}
}

// cohort builds n servers in a namespace, the first trueCount of them reading
// stateless-true, all healthy.
func cohort(ns string, n, trueCount int) []probe.Result {
	out := make([]probe.Result, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, res(ns+"/s"+string(rune('a'+i%26))+string(rune('a'+i/26)), probe.Healthy, i < trueCount))
	}
	return out
}

func TestDriftCandidatesFlagsAWholesaleFlip(t *testing.T) {
	// The shape of the 2026-09-09 incident in miniature: the verdicts hold,
	// the stateless readings collapse.
	prev := statsByNamespace(cohort("io.github.flaky", 100, 97))
	cur := statsByNamespace(cohort("io.github.flaky", 100, 20))

	got := driftCandidates(prev, cur, DefaultReprobeThreshold, DefaultReprobeMinServers)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d (%+v)", len(got), got)
	}
	if got[0].Namespace != "io.github.flaky" || got[0].Signal != "statelessAccepted" {
		t.Fatalf("wrong candidate: %+v", got[0])
	}
	if got[0].DeltaPoints < 70 {
		t.Fatalf("want a large delta, got %.1f", got[0].DeltaPoints)
	}
}

func TestDriftCandidatesIgnoresSmallNamespacesAndSmallMoves(t *testing.T) {
	// A 3-server namespace swings 33 points when one server blinks. That is
	// noise, and re-probing it would train the reader to ignore this report.
	small := driftCandidates(statsByNamespace(cohort("tiny", 3, 3)), statsByNamespace(cohort("tiny", 3, 1)),
		DefaultReprobeThreshold, DefaultReprobeMinServers)
	if len(small) != 0 {
		t.Fatalf("small namespace should not be a candidate: %+v", small)
	}

	// A move under the threshold is not drift.
	quiet := driftCandidates(statsByNamespace(cohort("steady", 100, 90)), statsByNamespace(cohort("steady", 100, 85)),
		DefaultReprobeThreshold, DefaultReprobeMinServers)
	if len(quiet) != 0 {
		t.Fatalf("a 5-point move should not be a candidate: %+v", quiet)
	}
}

func TestDriftCandidatesIgnoresNewNamespaces(t *testing.T) {
	// A namespace with no previous edition has nothing to have drifted from.
	got := driftCandidates(statsByNamespace(cohort("old", 100, 90)), statsByNamespace(cohort("brandnew", 100, 10)),
		DefaultReprobeThreshold, DefaultReprobeMinServers)
	if len(got) != 0 {
		t.Fatalf("a new namespace is not drift: %+v", got)
	}
}

func TestDriftCandidatesOrdersWorstFirst(t *testing.T) {
	// The MaxServers budget is spent in this order, so it must start with the
	// namespace that moved furthest.
	prev := statsByNamespace(append(cohort("a", 100, 95), cohort("b", 100, 95)...))
	cur := statsByNamespace(append(cohort("a", 100, 70), cohort("b", 100, 20)...))
	got := driftCandidates(prev, cur, DefaultReprobeThreshold, DefaultReprobeMinServers)
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(got))
	}
	if got[0].Namespace != "b" {
		t.Fatalf("want the larger move first, got %+v", got)
	}
}

func TestSignalOfTreatsAMissingReadinessAsNoAnswer(t *testing.T) {
	// Our own inability to read a signal is never an observation about the
	// target, so it must be distinguishable from a false reading.
	_, ok := signalOf(probe.Result{Name: "x/y"}, "statelessAccepted")
	if ok {
		t.Fatal("a result with no readiness pass must not answer a readiness signal")
	}
	// Health is always answerable: every result carries a verdict.
	if _, ok := signalOf(probe.Result{Name: "x/y", Verdict: probe.Dead}, "healthy"); !ok {
		t.Fatal("health must always be answerable")
	}
}

// Proves the selector against the census pair it was built for. Skipped unless
// both editions are on disk, because they live in the state-of-mcp data repo,
// not here:
//
//	AKASHI_CENSUS_PREV=.../2026-08-03 AKASHI_CENSUS_CUR=.../2026-09-09 go test ./internal/scan
func TestDriftCandidatesAgainstTheRealIncident(t *testing.T) {
	prevPath, curPath := os.Getenv("AKASHI_CENSUS_PREV"), os.Getenv("AKASHI_CENSUS_CUR")
	if prevPath == "" || curPath == "" {
		t.Skip("set AKASHI_CENSUS_PREV and AKASHI_CENSUS_CUR to run against real censuses")
	}
	prev, err := loadEdition(prevPath)
	if err != nil {
		t.Fatalf("load prev: %v", err)
	}
	cur, err := loadEdition(curPath)
	if err != nil {
		t.Fatalf("load cur: %v", err)
	}
	got := driftCandidates(statsByNamespace(prev), statsByNamespace(cur), DefaultReprobeThreshold, DefaultReprobeMinServers)
	var found *NamespaceDrift
	for i := range got {
		t.Logf("candidate: %s %s %.1f -> %.1f (%.1f points, %d servers)",
			got[i].Namespace, got[i].Signal, got[i].PreviousPct, got[i].CurrentPct, got[i].DeltaPoints, got[i].Servers)
		if got[i].Namespace == "io.github.pipeworx-io" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("io.github.pipeworx-io was the documented contamination and must be a candidate; got %d candidates", len(got))
	}
	if found.Signal != "statelessAccepted" {
		t.Fatalf("want statelessAccepted as the moved signal, got %q", found.Signal)
	}
}

// thirtyServers is one namespace big enough to clear DefaultReprobeMinServers.
func thirtyServersJSON() string {
	var b []string
	for i := 0; i < 30; i++ {
		b = append(b, `{"server": {"name":"drift/s`+twoLetter(i)+`"}, "_meta": {"io.modelcontextprotocol.registry/official": {"status":"active","isLatest":true}}}`)
	}
	return `{"servers": [` + joinComma(b) + `], "metadata": {"nextCursor": ""}}`
}

// twoLetter keeps generated names inside the registry charset for any i the
// tests use, rather than running off the end of the alphabet.
func twoLetter(i int) string {
	return string(rune('a'+i%26)) + string(rune('a'+i/26))
}

func joinComma(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func TestRunReprobesADriftingNamespaceWithoutTouchingTheCensus(t *testing.T) {
	out := t.TempDir()

	// A previous edition in which every server in the namespace was healthy.
	// The current scan probes servers that declare nothing, so their verdicts
	// cannot be healthy, which is a 100-point move on the health signal.
	prevDir := t.TempDir()
	var prev []byte
	for i := 0; i < 30; i++ {
		prev = append(prev, []byte(`{"name":"drift/s`+twoLetter(i)+`","verdict":"healthy","checkedAt":"2026-08-03"}`+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(prevDir, RecordsFile), prev, 0o600); err != nil {
		t.Fatal(err)
	}

	client := registryStub(t, thirtyServersJSON())
	eng := noNetworkEngine(t)

	summary, err := Run(context.Background(), client, eng, Options{Out: out, Compare: prevDir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if summary.Reprobe == nil {
		t.Fatal("summary must carry the re-probe report when --compare is set")
	}
	if summary.Reprobe.Candidates != 1 || len(summary.Reprobe.Namespaces) != 1 {
		t.Fatalf("want exactly one drifting namespace, got %+v", summary.Reprobe)
	}
	d := summary.Reprobe.Namespaces[0]
	if d.Namespace != "drift" || d.Signal != "healthy" {
		t.Fatalf("wrong drift: %+v", d)
	}
	if d.Reprobed != 30 {
		t.Fatalf("want 30 re-probed, got %d", d.Reprobed)
	}
	// The second reading saw the same thing the census did, which is the
	// honest answer here: nothing was flaky, the population really changed.
	if d.Agreed != 30 || d.Changed != 0 {
		t.Fatalf("want 30 agreed / 0 changed, got %d/%d", d.Agreed, d.Changed)
	}

	// The census file is the observation and is never edited by the second
	// pass: exactly the servers probed once, still one line each.
	if got := len(readRecordLines(t, out)); got != 30 {
		t.Fatalf("records.jsonl must still hold 30 lines, got %d", got)
	}
	if got := len(readLines(t, filepath.Join(out, ReprobeFile))); got != 30 {
		t.Fatalf("reprobe.jsonl must hold the 30 second readings, got %d", got)
	}
}

func TestRunWithoutCompareWritesNoReprobe(t *testing.T) {
	out := t.TempDir()
	client := registryStub(t, thirtyServersJSON())
	summary, err := Run(context.Background(), client, noNetworkEngine(t), Options{Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Reprobe != nil {
		t.Fatalf("no --compare must mean no re-probe report, got %+v", summary.Reprobe)
	}
	if _, err := os.Stat(filepath.Join(out, ReprobeFile)); !os.IsNotExist(err) {
		t.Fatal("no --compare must leave reprobe.jsonl absent")
	}
}
