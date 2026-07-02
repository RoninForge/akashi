package cli

import (
	"bytes"
	"strings"
	"testing"
)

// run drives Execute with captured writers, like main does.
func run(args ...string) (stdout, stderr string, code int) {
	var out, err bytes.Buffer
	code = Execute(&out, &err, args)
	return out.String(), err.String(), code
}

func TestVersionCommand(t *testing.T) {
	out, _, code := run("version")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "akashi") {
		t.Errorf("version output missing 'akashi': %q", out)
	}
}

func TestHelpExitsZero(t *testing.T) {
	_, _, code := run("--help")
	if code != 0 {
		t.Errorf("--help exit = %d, want 0", code)
	}
}

func TestCheckRequiresArg(t *testing.T) {
	_, _, code := run("check")
	if code != 2 {
		t.Errorf("check with no arg exit = %d, want 2", code)
	}
}

func TestCheckJSONAndBadgeAreMutuallyExclusive(t *testing.T) {
	// This error is returned before any network call.
	_, stderr, code := run("check", "anything", "--json", "--badge")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not both") {
		t.Errorf("stderr = %q, want a mutual-exclusion message", stderr)
	}
}

func TestUnknownCommand(t *testing.T) {
	_, _, code := run("frobnicate")
	if code != 2 {
		t.Errorf("unknown command exit = %d, want 2", code)
	}
}

func TestScanRequiresOut(t *testing.T) {
	// This error is returned before any network call: cobra's required-flag
	// check runs before RunE.
	_, stderr, code := run("scan")
	if code != 2 {
		t.Errorf("scan with no --out exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "out") {
		t.Errorf("stderr = %q, want a message about the required --out flag", stderr)
	}
}

func TestScanHelp(t *testing.T) {
	out, _, code := run("scan", "--help")
	if code != 0 {
		t.Errorf("scan --help exit = %d, want 0", code)
	}
	for _, want := range []string{"records.jsonl", "summary.json", "--out", "--concurrency"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan --help output missing %q", want)
		}
	}
}
