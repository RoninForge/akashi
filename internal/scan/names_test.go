package scan

import (
	"fmt"
	"sort"
	"testing"

	"github.com/RoninForge/akashi/internal/registry"
)

// namedServers builds a minimal []registry.Server carrying only names, which
// is all validateNames looks at.
func namedServers(names ...string) []registry.Server {
	out := make([]registry.Server, 0, len(names))
	for _, n := range names {
		out = append(out, registry.Server{Name: n})
	}
	return out
}

func TestValidateNamesAllCleanHasNoIssues(t *testing.T) {
	issues := validateNames(namedServers("io.github.acme/thing", "com.example.co/other-server_1.0"))
	if issues.hasIssues() {
		t.Fatalf("expected no issues, got %+v", issues)
	}
}

func TestValidateNamesBadCharset(t *testing.T) {
	tests := []struct {
		name string
		bad  string
	}{
		{"uppercase letter", "io.github.Acme/thing"},
		{"space", "io.github.acme/thing one"},
		{"non-ascii letter", "io.github.acme/thïng"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := validateNames(namedServers(tt.bad))
			if issues.BadCharset.Count != 1 {
				t.Errorf("badCharset count = %d, want 1", issues.BadCharset.Count)
			}
			if len(issues.BadCharset.Sample) != 1 || issues.BadCharset.Sample[0] != tt.bad {
				t.Errorf("badCharset sample = %v, want [%q]", issues.BadCharset.Sample, tt.bad)
			}
		})
	}
}

func TestValidateNamesBadShape(t *testing.T) {
	tests := []struct {
		name string
		bad  string
	}{
		{"no slash", "io.github.acme-thing"},
		{"two slashes", "io.github.acme/thing/extra"},
		{"namespace has no dot", "acme/thing"},
		{"namespace starts with dot", ".acme.io/thing"},
		{"local segment starts with dot", "io.github.acme/.thing"},
		{"empty local segment", "io.github.acme/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := validateNames(namedServers(tt.bad))
			if issues.BadShape.Count != 1 {
				t.Errorf("badShape count = %d, want 1", issues.BadShape.Count)
			}
			if len(issues.BadShape.Sample) != 1 || issues.BadShape.Sample[0] != tt.bad {
				t.Errorf("badShape sample = %v, want [%q]", issues.BadShape.Sample, tt.bad)
			}
		})
	}
}

func TestValidateNamesWellShapedCleanNameIsNotFlagged(t *testing.T) {
	issues := validateNames(namedServers("io.github.acme/thing"))
	if issues.BadShape.Count != 0 || issues.BadCharset.Count != 0 {
		t.Errorf("expected a well-shaped, clean-charset name to pass both checks, got %+v", issues)
	}
}

func TestValidateNamesCaseInsensitiveCollision(t *testing.T) {
	issues := validateNames(namedServers("io.github.acme/thing", "io.github.acme/Thing", "io.github.acme/other"))
	if issues.CaseCollisions.Count != 2 {
		t.Fatalf("caseCollisions count = %d, want 2 (both names in the colliding pair)", issues.CaseCollisions.Count)
	}
	got := append([]string(nil), issues.CaseCollisions.Sample...)
	sort.Strings(got)
	want := []string{"io.github.acme/Thing", "io.github.acme/thing"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("caseCollisions sample = %v, want %v", got, want)
	}
	// "io.github.acme/other" does not collide with anything.
	for _, n := range issues.CaseCollisions.Sample {
		if n == "io.github.acme/other" {
			t.Errorf("caseCollisions sample wrongly includes a non-colliding name %q", n)
		}
	}
}

func TestValidateNamesNoFalseCollisionOnExactDuplicate(t *testing.T) {
	// registry.Drain already dedupes exact-name duplicates before
	// validateNames ever sees them, but a case-sensitive-identical repeat
	// here must not be miscounted as a collision either.
	issues := validateNames(namedServers("io.github.acme/thing", "io.github.acme/thing"))
	if issues.CaseCollisions.Count != 0 {
		t.Errorf("caseCollisions count = %d, want 0 for two identical names", issues.CaseCollisions.Count)
	}
}

func TestValidateNamesSampleCappedAtTwenty(t *testing.T) {
	names := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		names = append(names, fmt.Sprintf("bad name %02d", i)) // the space makes every one a badCharset hit
	}
	issues := validateNames(namedServers(names...))
	if issues.BadCharset.Count != 25 {
		t.Fatalf("badCharset count = %d, want 25", issues.BadCharset.Count)
	}
	if len(issues.BadCharset.Sample) != nameIssueSampleCap {
		t.Errorf("badCharset sample len = %d, want the %d cap", len(issues.BadCharset.Sample), nameIssueSampleCap)
	}
}

func TestValidateNamesEmptyPopulation(t *testing.T) {
	issues := validateNames(nil)
	if issues.hasIssues() {
		t.Errorf("expected no issues for an empty population, got %+v", issues)
	}
}
