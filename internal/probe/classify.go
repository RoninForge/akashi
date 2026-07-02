package probe

import (
	"fmt"

	"github.com/RoninForge/akashi/internal/registry"
)

// classify turns raw signals into a verdict, the reasons behind it, and a
// display-ready check list. It mirrors the PoC engine's rules exactly:
//
//   - dead     : registry status "deleted", or every probed entrypoint broken
//   - unknown  : nothing probeable was declared
//   - degraded : at least one live entrypoint but something is broken
//   - healthy  : at least one live entrypoint and nothing broken
func classify(s registry.Server, sig Signals) Result {
	var reasons []string

	repo := sig.Repo
	repoAlive := repo.Status == "alive"
	switch repo.Status {
	case "missing":
		reasons = append(reasons, "repo_404")
	case "archived":
		reasons = append(reasons, "repo_archived")
	}
	if repoAlive && repo.AgeDays != nil && *repo.AgeDays > staleDays {
		reasons = append(reasons, "repo_stale_1yr")
	}

	pkgPublished := 0
	for _, p := range sig.Packages {
		switch p.Status {
		case "published":
			pkgPublished++
		case "missing":
			reasons = append(reasons, p.Type+"_missing")
		case "unpublished":
			reasons = append(reasons, p.Type+"_unpublished")
		}
	}

	remoteReachable := 0
	for _, r := range sig.Remotes {
		switch r.Status {
		case "reachable":
			remoteReachable++
			// A remote that answered 2xx but is not a real MCP server (an HTML
			// page or proxy) is reachable but a conformance failure: it must
			// not earn a green badge. auth_gated and unverified (transport we
			// could not exercise) are alive-and-fine, so they do not downgrade.
			if r.Conformance == "reachable_nonconformant" {
				reasons = append(reasons, "remote_nonconformant")
			}
		case "unreachable":
			reasons = append(reasons, "remote_unreachable")
		case "server_error":
			reasons = append(reasons, "remote_5xx")
		case "not_found":
			reasons = append(reasons, "remote_404")
		}
	}

	// A deprecated (but not deleted) registry entry is a real defect for a
	// single-server trust check: it must not earn a green "verified" badge. We
	// flag it before computing the verdict so a working-but-deprecated server
	// resolves to degraded rather than healthy. (This is a deliberate departure
	// from the population-probe PoC, which flagged deprecation only after the
	// verdict; the population index can apply its own aggregation policy.)
	if s.RegistryStatus == "deprecated" {
		reasons = append(reasons, "registry_deprecated")
	}

	// Entrypoints we could actually evaluate, and how many are alive.
	probed := 0
	if repo.Status == "alive" || repo.Status == "archived" || repo.Status == "missing" {
		probed++
	}
	for _, p := range sig.Packages {
		switch p.Status {
		case "published", "missing", "unpublished":
			probed++
		}
	}
	for _, r := range sig.Remotes {
		switch r.Status {
		case "reachable", "unreachable", "server_error", "not_found":
			probed++
		}
	}
	alive := boolToInt(repoAlive) + pkgPublished + remoteReachable

	var verdict Verdict
	switch {
	case s.RegistryStatus == "deleted":
		verdict = Dead
	case probed == 0:
		verdict = Unknown
	case alive == 0:
		verdict = Dead
	case len(reasons) > 0:
		verdict = Degraded
	default:
		verdict = Healthy
	}

	return Result{
		Name:              s.Name,
		RegistryStatus:    s.RegistryStatus,
		Version:           s.Version,
		Verdict:           verdict,
		Reasons:           dedupe(reasons),
		Checks:            buildChecks(s, sig, alive),
		Signals:           sig,
		AliveEntrypoints:  alive,
		ProbedEntrypoints: probed,
	}
}

// buildChecks renders the signals as a human-readable pass/warn/fail list. A
// check that does not apply to the resolved target (for example registry
// status for a bare repo URL) is Skip.
func buildChecks(s registry.Server, sig Signals, alive int) []Check {
	var checks []Check

	// Registry status.
	switch s.RegistryStatus {
	case "":
		checks = append(checks, Check{Name: "registry status", Status: Skip, Detail: "not resolved from the registry"})
	case "active":
		checks = append(checks, Check{Name: "registry status", Status: Pass, Detail: "active"})
	case "deprecated":
		checks = append(checks, Check{Name: "registry status", Status: Warn, Detail: "deprecated"})
	case "deleted":
		checks = append(checks, Check{Name: "registry status", Status: Fail, Detail: "deleted"})
	default:
		checks = append(checks, Check{Name: "registry status", Status: Warn, Detail: s.RegistryStatus})
	}

	// Repository reachability + freshness.
	repo := sig.Repo
	switch repo.Status {
	case "none":
		checks = append(checks, Check{Name: "repo reachable", Status: Skip, Detail: "no repository declared"})
	case "unprobed":
		checks = append(checks, Check{Name: "repo reachable", Status: Skip, Detail: "non-GitHub host, not probed"})
	case "alive":
		checks = append(checks, Check{Name: "repo reachable", Status: Pass, Detail: "exists"})
		checks = append(checks, freshnessCheck(repo))
	case "archived":
		checks = append(checks, Check{Name: "repo reachable", Status: Warn, Detail: "archived (read-only)"})
	case "missing":
		checks = append(checks, Check{Name: "repo reachable", Status: Fail, Detail: "404 (repository gone)"})
	default:
		checks = append(checks, Check{Name: "repo reachable", Status: Warn, Detail: "probe error: " + repo.Detail})
	}

	// Packages.
	for _, p := range sig.Packages {
		name := "package " + p.Type
		switch p.Status {
		case "published":
			d := "published"
			if p.Latest != "" {
				d = "published (" + p.Latest + ")"
			}
			checks = append(checks, Check{Name: name, Status: Pass, Detail: d})
		case "missing":
			checks = append(checks, Check{Name: name, Status: Fail, Detail: p.ID + " not found"})
		case "unpublished":
			checks = append(checks, Check{Name: name, Status: Fail, Detail: p.ID + " unpublished"})
		case "unprobed":
			checks = append(checks, Check{Name: name, Status: Skip, Detail: p.ID + " not keyless-probeable"})
		default:
			checks = append(checks, Check{Name: name, Status: Warn, Detail: "probe error"})
		}
	}

	// Remotes: reachability, then conformance.
	for _, r := range sig.Remotes {
		switch r.Status {
		case "reachable":
			checks = append(checks, Check{Name: "remote reachable", Status: Pass, Detail: httpDetail(r)})
			checks = append(checks, conformanceCheck(r))
		case "unreachable":
			checks = append(checks, Check{Name: "remote reachable", Status: Fail, Detail: "unreachable"})
		case "server_error":
			checks = append(checks, Check{Name: "remote reachable", Status: Fail, Detail: fmt.Sprintf("server error (HTTP %d)", r.HTTPStatus)})
		case "not_found":
			checks = append(checks, Check{Name: "remote reachable", Status: Fail, Detail: "404 (endpoint gone)"})
		default:
			checks = append(checks, Check{Name: "remote reachable", Status: Warn, Detail: r.Status})
		}
	}

	// At least one live entrypoint (the bottom-line liveness).
	if alive > 0 {
		checks = append(checks, Check{Name: "at least one live entrypoint", Status: Pass, Detail: fmt.Sprintf("%d alive", alive)})
	} else {
		checks = append(checks, Check{Name: "at least one live entrypoint", Status: Fail, Detail: "nothing installable or reachable"})
	}

	// License (conformance signal, free from the GitHub API).
	if repo.Status == "alive" || repo.Status == "archived" {
		if repo.License != "" {
			checks = append(checks, Check{Name: "license present", Status: Pass, Detail: repo.License})
		} else {
			checks = append(checks, Check{Name: "license present", Status: Warn, Detail: "none detected"})
		}
	}

	return checks
}

func freshnessCheck(repo RepoSignal) Check {
	if repo.AgeDays == nil {
		return Check{Name: "repo freshness", Status: Skip, Detail: "no push date"}
	}
	age := *repo.AgeDays
	switch {
	case age <= warnDays:
		return Check{Name: "repo freshness", Status: Pass, Detail: fmt.Sprintf("pushed %dd ago", age)}
	case age <= staleDays:
		return Check{Name: "repo freshness", Status: Warn, Detail: fmt.Sprintf("no push in %dd", age)}
	default:
		return Check{Name: "repo freshness", Status: Fail, Detail: fmt.Sprintf("no push in %dd (>1y)", age)}
	}
}

func conformanceCheck(r RemoteSignal) Check {
	switch r.Conformance {
	case "initialize_ok":
		if r.IDEchoed != nil && !*r.IDEchoed {
			return Check{Name: "MCP conformance", Status: Warn, Detail: "handshake ok but JSON-RPC id not echoed"}
		}
		return Check{Name: "MCP conformance", Status: Pass, Detail: "initialize handshake ok"}
	case "auth_gated":
		return Check{Name: "MCP conformance", Status: Warn, Detail: fmt.Sprintf("auth-gated (HTTP %d): alive, not verifiable keyless", r.HTTPStatus)}
	case "reachable_nonconformant":
		return Check{Name: "MCP conformance", Status: Fail, Detail: fmt.Sprintf("returned HTTP %d but is not an MCP server", r.HTTPStatus)}
	case "unverified":
		return Check{Name: "MCP conformance", Status: Warn, Detail: fmt.Sprintf("endpoint up (HTTP %d) but did not accept an initialize over this transport", r.HTTPStatus)}
	default:
		return Check{Name: "MCP conformance", Status: Skip, Detail: "not evaluated"}
	}
}

func httpDetail(r RemoteSignal) string {
	if r.HTTPStatus > 0 {
		return fmt.Sprintf("HTTP %d via %s", r.HTTPStatus, r.Probe)
	}
	return "via " + r.Probe
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
