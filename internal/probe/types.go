// Package probe is the keyless MCP-server health engine. For a given server it
// runs only public, unauthenticated signals - repository liveness (GitHub API),
// package publication (npm, PyPI, Docker Hub anon), and remote reachability
// (a plain GET plus a capability-only MCP `initialize` handshake) - and
// classifies the server as healthy, degraded, dead, or unknown.
//
// It never authenticates to a probed server and never executes a tool. Any
// GitHub token supplied is used solely against the public GitHub API as a
// higher-rate-limit measurement instrument, exactly as a human running `gh`
// would. Nothing here touches a user secret. This keeps the zero-key pledge
// true by construction.
package probe

// Verdict is the overall health classification of a server.
type Verdict string

// Verdict values, from best to worst health.
const (
	Healthy  Verdict = "healthy"  // at least one live entrypoint and nothing broken
	Degraded Verdict = "degraded" // usable, but something is broken
	Dead     Verdict = "dead"     // registry-deleted, or every probed entrypoint is broken
	Unknown  Verdict = "unknown"  // only un-probeable entrypoints declared
)

// Status is a single check outcome for display.
type Status string

// Status values for a single check line.
const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one line in the human-readable report.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// RepoSignal is the raw repository probe result.
type RepoSignal struct {
	Kind       string `json:"kind"`   // github|other|none
	Status     string `json:"status"` // alive|archived|missing|error|unprobed|none
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Archived   bool   `json:"archived,omitempty"`
	PushedAt   string `json:"pushedAt,omitempty"`
	AgeDays    *int   `json:"ageDays,omitempty"`
	Stars      *int   `json:"stars,omitempty"`
	License    string `json:"license,omitempty"`
	URL        string `json:"url,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// PackageSignal is the raw package probe result for one entrypoint.
type PackageSignal struct {
	Type       string `json:"type"` // npm|pypi|oci
	ID         string `json:"id"`
	Status     string `json:"status"` // published|missing|unpublished|unprobed|error
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Latest     string `json:"latest,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// RemoteSignal is the raw remote-endpoint probe result.
type RemoteSignal struct {
	URL         string `json:"url"`
	Type        string `json:"type,omitempty"`
	Status      string `json:"status"` // reachable|unreachable|server_error|not_found
	HTTPStatus  int    `json:"httpStatus,omitempty"`
	Conformance string `json:"conformance,omitempty"` // initialize_ok|auth_gated|reachable_nonconformant
	Probe       string `json:"probe,omitempty"`       // get|initialize
	IDEchoed    *bool  `json:"idEchoed,omitempty"`    // did the initialize response echo the JSON-RPC id
	Detail      string `json:"detail,omitempty"`
	// tools/list probe, run with the official MCP client after a conformant
	// initialize. A completed session that lists tools is the strongest keyless
	// proof that this is a real, working MCP server.
	ToolsStatus string   `json:"toolsStatus,omitempty"` // ok|empty|error|connect_failed
	ToolCount   int      `json:"toolCount,omitempty"`
	ToolNames   []string `json:"toolNames,omitempty"`
}

// ServerJSONSignal is the result of validating the server's published
// server.json against its declared JSON Schema.
type ServerJSONSignal struct {
	Status string   `json:"status"` // valid|invalid|no_schema|absent|error
	Schema string   `json:"schema,omitempty"`
	Errors []string `json:"errors,omitempty"`
}

// Signals is the full raw evidence behind a verdict.
type Signals struct {
	Repo       RepoSignal       `json:"repo"`
	Packages   []PackageSignal  `json:"packages"`
	Remotes    []RemoteSignal   `json:"remotes"`
	ServerJSON ServerJSONSignal `json:"serverJson"`
}

// Result is the per-server probe outcome: a verdict, the reasons behind it,
// a display-ready check list, and the raw signals for machine consumers.
type Result struct {
	Name              string   `json:"name"`
	RegistryStatus    string   `json:"registryStatus,omitempty"`
	Version           string   `json:"version,omitempty"`
	Verdict           Verdict  `json:"verdict"`
	Reasons           []string `json:"reasons"`
	Checks            []Check  `json:"checks"`
	Signals           Signals  `json:"signals"`
	AliveEntrypoints  int      `json:"aliveEntrypoints"`
	ProbedEntrypoints int      `json:"probedEntrypoints"`
	CheckedAt         string   `json:"checkedAt"` // YYYY-MM-DD, UTC
}

// OK reports whether the verdict is one a badge should render green.
func (r Result) OK() bool { return r.Verdict == Healthy }
