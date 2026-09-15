package scan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RoninForge/akashi/internal/probe"
	"github.com/RoninForge/akashi/internal/report"
)

// RecordsDir is the sharded records directory written inside --out. It
// replaces the single RecordsFile, which GitHub warns on past 50MiB and
// refuses past 100MiB: one census crossed 51.5MiB at 31,538 servers, and the
// hard limit lands near 47,700 at the same bytes-per-server.
const RecordsDir = "records"

// RecordsManifestFile names the shard listing written inside RecordsDir.
const RecordsManifestFile = "manifest.json"

// RecordShards is how many shard files a census is split across. All records
// for one namespace land in the same shard, so a consumer wanting one
// operator fetches one small file instead of the whole census.
const RecordShards = 64

// shardIndex maps a server name to its shard, keyed on the namespace so a
// namespace is never split. FNV-1a keeps the mapping stable across machines
// and Go versions, which matters because the shard a record lives in is part
// of the published dataset's shape.
func shardIndex(name string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(namespaceOf(name)))
	return int(h.Sum32() % RecordShards)
}

// ShardFileName is the shard filename a server's record belongs in, relative
// to RecordsDir. Exported so a consumer can resolve a name to one file.
func ShardFileName(name string) string {
	return fmt.Sprintf("%02d.jsonl", shardIndex(name))
}

// recordWriter appends probe results to the shard files under RecordsDir,
// opening each shard lazily and keeping it open for the run.
type recordWriter struct {
	dir   string
	files map[int]*os.File
}

func newRecordWriter(outDir string) (*recordWriter, error) {
	dir := filepath.Join(outDir, RecordsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("scan: create %s: %w", dir, err)
	}
	return &recordWriter{dir: dir, files: make(map[int]*os.File, RecordShards)}, nil
}

func (w *recordWriter) Write(r probe.Result) error {
	i := shardIndex(r.Name)
	f, ok := w.files[i]
	if !ok {
		path := filepath.Join(w.dir, fmt.Sprintf("%02d.jsonl", i))
		// #nosec G304 -- path is derived from --out, an operator-supplied CLI
		// flag naming where to write the dataset.
		opened, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("scan: open %s: %w", path, err)
		}
		w.files[i] = opened
		f = opened
	}
	if err := report.WriteJSONLine(f, r); err != nil {
		return fmt.Errorf("scan: write record for %s: %w", r.Name, err)
	}
	return f.Sync()
}

// Close closes every shard file the run opened, returning the first error. It
// is idempotent: Run closes explicitly before writing the manifest and again
// from a defer on the error paths.
func (w *recordWriter) Close() error {
	var firstErr error
	for i, f := range w.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(w.files, i)
	}
	return firstErr
}

// RecordPaths returns every records file for a census directory, newest
// layout first: the shard files under RecordsDir when that exists, otherwise
// the legacy single RecordsFile. Censuses published before sharding keep
// working with no migration.
func RecordPaths(censusDir string) ([]string, error) {
	dir := filepath.Join(censusDir, RecordsDir)
	entries, err := os.ReadDir(dir)
	if err == nil {
		paths := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				paths = append(paths, filepath.Join(dir, e.Name()))
			}
		}
		if len(paths) > 0 {
			sort.Strings(paths)
			return paths, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	legacy := filepath.Join(censusDir, RecordsFile)
	// #nosec G703 -- censusDir is --out or --compare, an operator-supplied CLI
	// flag naming a census directory to read.
	if _, err := os.Stat(legacy); err == nil {
		return []string{legacy}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return nil, nil
}

// readRecordFile adds every parseable record in path to done. A line that
// fails to parse (a partial write left by a hard kill mid-record) is skipped
// rather than treated as fatal: it is simply not counted as done, so a later
// run re-probes that one server and appends a fresh record after it.
func readRecordFile(path string, done map[string]probe.Result) error {
	// #nosec G304,G703 -- path names a dataset file to read, and comes from
	// --out or --compare: operator-supplied CLI flags.
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

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
	return sc.Err()
}

// ReadRecords reads every record of a census directory, keyed by server name,
// across whichever layout that census uses.
func ReadRecords(censusDir string) (map[string]probe.Result, error) {
	paths, err := RecordPaths(censusDir)
	if err != nil {
		return nil, err
	}
	done := make(map[string]probe.Result)
	for _, p := range paths {
		if err := readRecordFile(p, done); err != nil {
			return nil, err
		}
	}
	return done, nil
}

// RecordsManifest is the shard listing published alongside the shards, so a
// consumer can enumerate the census without a directory listing and resolve
// one namespace to one file without reimplementing the hash.
type RecordsManifest struct {
	SchemaVersion string            `json:"schemaVersion"`
	Algorithm     string            `json:"algorithm"`
	Shards        int               `json:"shards"`
	Total         int               `json:"total"`
	Files         []RecordsShard    `json:"files"`
	Namespaces    map[string]string `json:"namespaces"`
}

// RecordsShard is one shard's entry in the manifest.
type RecordsShard struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

// writeRecordsManifest writes RecordsManifestFile describing the shards that
// hold results.
func writeRecordsManifest(outDir string, results []probe.Result) error {
	// Counted by distinct name, not by slice length: the manifest total is what
	// a consumer trusts as the census size, so it must equal what a reader of
	// the shards actually sees. A repeated name is one record on disk.
	counts := make(map[int]int, RecordShards)
	namespaces := make(map[string]string)
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		if seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		i := shardIndex(r.Name)
		counts[i]++
		namespaces[namespaceOf(r.Name)] = fmt.Sprintf("%02d.jsonl", i)
	}
	files := make([]RecordsShard, 0, len(counts))
	for i := 0; i < RecordShards; i++ {
		if counts[i] == 0 {
			continue
		}
		files = append(files, RecordsShard{File: fmt.Sprintf("%02d.jsonl", i), Count: counts[i]})
	}
	m := RecordsManifest{
		SchemaVersion: "1.0.0",
		Algorithm:     "fnv1a32(namespace) % shards",
		Shards:        RecordShards,
		Total:         len(seen),
		Files:         files,
		Namespaces:    namespaces,
	}
	path := filepath.Join(outDir, RecordsDir, RecordsManifestFile)
	// #nosec G304 -- path is derived from --out, an operator-supplied CLI flag.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("scan: create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("scan: write %s: %w", path, err)
	}
	return nil
}

// readShardDir adds every record in a records/ directory to done, for an
// operator who points --compare at the shard dir rather than the census dir.
func readShardDir(dir string, done map[string]probe.Result) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if err := readRecordFile(filepath.Join(dir, n), done); err != nil {
			return err
		}
	}
	return nil
}
