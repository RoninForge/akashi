package scan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RoninForge/akashi/internal/probe"
)

// --compare has to accept a sharded census, its records/ dir, and a legacy
// single file, because an operator points it at whichever they have to hand
// and a census published before sharding must stay comparable forever.
func TestLoadEditionAcceptsEveryLayout(t *testing.T) {
	sharded := t.TempDir()
	w, err := newRecordWriter(sharded)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"io.github.a/one", "io.github.b/two", "io.github.c/three"} {
		if err := w.Write(probe.Result{Name: n, Verdict: "healthy"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	legacy := t.TempDir()
	body := `{"name":"io.github.a/one","verdict":"healthy"}` + "\n" +
		`{"name":"io.github.b/two","verdict":"healthy"}` + "\n" +
		`{"name":"io.github.c/three","verdict":"healthy"}` + "\n"
	if err := os.WriteFile(filepath.Join(legacy, RecordsFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"sharded census dir":  sharded,
		"sharded records dir": filepath.Join(sharded, RecordsDir),
		"legacy census dir":   legacy,
		"legacy records file": filepath.Join(legacy, RecordsFile),
	} {
		got, err := loadEdition(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(got) != 3 {
			t.Errorf("%s: read %d records, want 3", name, len(got))
		}
	}
}
