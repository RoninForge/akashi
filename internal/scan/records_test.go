package scan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/RoninForge/akashi/internal/probe"
)

// A namespace must never be split across shards: a consumer that fetches one
// shard for one operator has to get all of that operator's records or the
// per-namespace fetch is a silent partial read.
func TestShardKeepsANamespaceWhole(t *testing.T) {
	want := ShardFileName("io.github.pipeworx-io/first")
	for _, leaf := range []string{"second", "third", "a-very-long-leaf-name", "x"} {
		name := "io.github.pipeworx-io/" + leaf
		if got := ShardFileName(name); got != want {
			t.Errorf("%s landed in %s, want %s with the rest of its namespace", name, got, want)
		}
	}
}

// The shard mapping is part of the published dataset's shape, so it must not
// drift: a record that moves shard between releases breaks every consumer
// that cached a per-namespace URL.
func TestShardMappingIsPinned(t *testing.T) {
	for name, want := range map[string]string{
		"io.github.pipeworx-io/mcp": "43.jsonl",
		"io.github.mcp-dir/server":  "06.jsonl",
		"ai.aisecuritygateway/mcp":  "19.jsonl",
	} {
		if got := ShardFileName(name); got != want {
			t.Errorf("ShardFileName(%q) = %s, want %s (the mapping is published; changing it is a breaking change)", name, got, want)
		}
	}
}

func TestShardsSpreadAcrossFiles(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		seen[ShardFileName("io.github.ns"+string(rune('a'+i%26))+string(rune('a'+i/26))+"/x")] = true
	}
	if len(seen) < RecordShards/2 {
		t.Errorf("500 namespaces landed in only %d of %d shards; the hash is not spreading", len(seen), RecordShards)
	}
}

// A census published before sharding must keep reading, or every historical
// edition becomes unreadable the day the writer changes.
func TestReadRecordsFallsBackToTheLegacyFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, RecordsFile)
	body := `{"name":"io.github.a/one","verdict":"healthy"}` + "\n" + `{"name":"io.github.b/two","verdict":"dead"}` + "\n"
	if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["io.github.a/one"].Verdict != "healthy" || got["io.github.b/two"].Verdict != "dead" {
		t.Fatalf("legacy records.jsonl not read back: %+v", got)
	}
}

// The sharded layout wins when both are present, so a re-scan into a dir that
// still holds an old records.jsonl does not double-count.
func TestShardedLayoutWinsOverLegacy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RecordsFile), []byte(`{"name":"io.github.stale/x","verdict":"dead"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := newRecordWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(probe.Result{Name: "io.github.fresh/y", Verdict: "healthy"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := got["io.github.stale/x"]; stale {
		t.Error("the legacy file was read even though shards exist; a re-scan would double-count")
	}
	if len(got) != 1 {
		t.Fatalf("want only the sharded record, got %d", len(got))
	}
}

// The manifest is what a consumer enumerates the census by, so its counts
// must equal what the shards actually hold.
func TestManifestCountsMatchTheShards(t *testing.T) {
	dir := t.TempDir()
	w, err := newRecordWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	var results []probe.Result
	for i := 0; i < 120; i++ {
		r := probe.Result{Name: "io.github.ns" + string(rune('a'+i%17)) + "/leaf" + string(rune('a'+i%7)), Verdict: "healthy"}
		results = append(results, r)
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeRecordsManifest(dir, results); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, RecordsDir, RecordsManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m RecordsManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	onDisk, err := ReadRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 120 writes over 119 distinct names: the manifest must report 119, the
	// number a reader of the shards sees, not the number of lines written.
	if m.Total != len(onDisk) {
		t.Errorf("manifest total %d but shards hold %d distinct records", m.Total, len(onDisk))
	}

	sum := 0
	for _, f := range m.Files {
		sum += f.Count
		if _, err := os.Stat(filepath.Join(dir, RecordsDir, f.File)); err != nil {
			t.Errorf("manifest lists %s but it is not on disk", f.File)
		}
	}
	if sum != m.Total {
		t.Errorf("manifest shard counts sum to %d, total says %d", sum, m.Total)
	}
	// Every namespace the manifest maps must resolve to the shard the hash picks.
	for ns, file := range m.Namespaces {
		if got := ShardFileName(ns + "/anything"); got != file {
			t.Errorf("manifest maps %s to %s but the hash says %s", ns, file, got)
		}
	}
}
