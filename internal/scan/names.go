package scan

import (
	"regexp"
	"sort"
	"strings"

	"github.com/RoninForge/akashi/internal/registry"
)

// nameIssueSampleCap bounds how many offending names summary.json keeps per
// nameIssues category. The count alone already conveys scale; keeping every
// name for a badly shaped population would bloat the summary for no benefit.
const nameIssueSampleCap = 20

// nameCharsetRe is the character set the planned <namespace>/<name> on-disk
// page routing tolerates: lowercase ASCII, digits, dot, underscore, and
// hyphen, plus the single "/" that separates the two segments. It is
// stricter than the registry's own name pattern (which permits uppercase),
// because a page path becomes a literal file path and macOS/APFS dev builds
// are case-insensitive, so an uppercase letter is itself a routing hazard,
// not just a style nit.
var nameCharsetRe = regexp.MustCompile(`^[a-z0-9._/-]+$`)

// NameIssue is one validation category over the drained server names: how
// many names fell into it, and a bounded sample of the offending names for
// spot-checking.
type NameIssue struct {
	Count  int      `json:"count"`
	Sample []string `json:"sample,omitempty"`
}

// NameIssues summarizes registry names that would not survive the planned
// two-segment <namespace>/<name> page routing, or that would collide on a
// case-insensitive filesystem once rendered to that path. It is purely
// informational: see validateNames, which never fails a scan over it.
type NameIssues struct {
	// BadCharset counts names containing a character outside
	// nameCharsetRe (for example an uppercase letter or a space).
	BadCharset NameIssue `json:"badCharset"`
	// BadShape counts names that are not exactly one "/" splitting a
	// non-empty namespace segment (containing at least one ".") from a
	// non-empty local segment, with neither segment starting with ".".
	BadShape NameIssue `json:"badShape"`
	// CaseCollisions counts names that are distinct but identical to
	// another name in this run once lowercased, which would collide as
	// the same file. An exact (case-sensitive) repeat of the same name is
	// not a collision by itself; registry.Drain already dedupes those
	// before validateNames ever sees the list.
	CaseCollisions NameIssue `json:"caseCollisions"`
}

// hasIssues reports whether any category found at least one offending name.
func (n NameIssues) hasIssues() bool {
	return n.BadCharset.Count > 0 || n.BadShape.Count > 0 || n.CaseCollisions.Count > 0
}

// validateNames checks every drained server name against the two-segment
// <namespace>/<name> path the index build will route it to, and flags any
// name that would collide with another on a case-insensitive filesystem. It
// never mutates or drops a server: aborting a 13,000+ server census over one
// bad registry name would be wrong, so this only surfaces what it finds for
// summary.json.
func validateNames(servers []registry.Server) NameIssues {
	var issues NameIssues

	lowerSeen := make(map[string][]string, len(servers))
	for _, s := range servers {
		if !nameCharsetRe.MatchString(s.Name) {
			issues.BadCharset.Count++
			issues.BadCharset.Sample = appendSample(issues.BadCharset.Sample, s.Name)
		}
		if !wellShapedName(s.Name) {
			issues.BadShape.Count++
			issues.BadShape.Sample = appendSample(issues.BadShape.Sample, s.Name)
		}

		lower := strings.ToLower(s.Name)
		lowerSeen[lower] = appendUnique(lowerSeen[lower], s.Name)
	}

	// A group of two or more original names sharing the same lowercased form
	// would all render to the same on-disk path; every name in that group is
	// a collision, not just the second one onward. Walk the groups in sorted
	// key order rather than raw map order: registry.Drain already sorts its
	// output for a reproducible dataset, and randomizing which names land in
	// a capped sample would throw that away.
	lowers := make([]string, 0, len(lowerSeen))
	for lower := range lowerSeen {
		lowers = append(lowers, lower)
	}
	sort.Strings(lowers)
	for _, lower := range lowers {
		names := lowerSeen[lower]
		if len(names) < 2 {
			continue
		}
		issues.CaseCollisions.Count += len(names)
		for _, n := range names {
			issues.CaseCollisions.Sample = appendSample(issues.CaseCollisions.Sample, n)
		}
	}

	return issues
}

// wellShapedName reports whether name splits into exactly two "/"-separated
// segments, with a namespace (before "/") that contains at least one "."
// and neither segment starting with ".".
func wellShapedName(name string) bool {
	segments := strings.Split(name, "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return false
	}
	namespace, local := segments[0], segments[1]
	if !strings.Contains(namespace, ".") {
		return false
	}
	if strings.HasPrefix(namespace, ".") || strings.HasPrefix(local, ".") {
		return false
	}
	return true
}

// appendSample appends name to sample unless it is already at
// nameIssueSampleCap.
func appendSample(sample []string, name string) []string {
	if len(sample) >= nameIssueSampleCap {
		return sample
	}
	return append(sample, name)
}

// appendUnique appends name to list unless it is already present. Server
// names arriving here are already deduped exactly (registry.Drain keys on
// the exact name), so in practice this only ever guards against a
// case-sensitive-identical repeat in a hand-built test fixture; it keeps
// validateNames correct regardless.
func appendUnique(list []string, name string) []string {
	for _, existing := range list {
		if existing == name {
			return list
		}
	}
	return append(list, name)
}
