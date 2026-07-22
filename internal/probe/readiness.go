// The 2026-07-28 spec-readiness pass. It runs only against a remote that
// already answered a conformant initialize, and adds a handful of read-only,
// list-level JSON-RPC calls: a stateless tools/list, server/discover, a
// header-mismatch check, a subscriptions/listen existence check, one GET, a
// resources/read of a sentinel URI that cannot exist, and a fetch of the
// public OAuth protected-resource metadata. No call authenticates and no call
// executes a tool, so the zero-key pledge holds by construction; the sentinel
// read mirrors what mcp-spec-check ships as a standard check.
//
// Ruleset: strategy/mcp-readiness-2026-07/RULESET.md in the RoninForge
// planning repo, compiled from the official 2026-07-28 draft changelog. The
// spec is a release candidate until 2026-07-28; RulesetVersion pins which
// revision of the rules produced a verdict, and the numeric error codes
// accept both the RC and the renumbered final values.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// RulesetVersion identifies the readiness ruleset a verdict was computed
// under. Bump to "2026-07-28" after re-diffing the final changelog on
// publication day.
const RulesetVersion = "2026-07-28-rc"

// readinessCallTimeout bounds each individual readiness request, so the whole
// pass stays inside a census's per-server budget even against a slow server.
const readinessCallTimeout = 8 * time.Second

// Readiness verdict values, per the ruleset: first match wins in the order
// at-risk, ready, needs-migration. A server with no conformant remote gets no
// ReadinessSignal at all, which is the "unknown" bucket.
const (
	ReadinessReady          = "ready"
	ReadinessNeedsMigration = "needs-migration"
	ReadinessAtRisk         = "at-risk"
)

// ReadinessSignal is the raw 2026-07-28 readiness evidence for one server,
// gathered from its first conformant remote, plus the verdict derived from
// it. Every field is an observable; the verdict is a pure function of them.
type ReadinessSignal struct {
	RulesetVersion string `json:"rulesetVersion"`
	URL            string `json:"url"`

	// Stateless core (B2, B3): does the server answer without a handshake,
	// and does it implement the new required discovery RPC.
	StatelessAccepted bool     `json:"statelessAccepted"`
	DiscoverSupported bool     `json:"discoverSupported"`
	DiscoverVersions  []string `json:"discoverVersions,omitempty"`

	// Sessions (B1): whether the initialize response minted an
	// Mcp-Session-Id, and whether requests are rejected without it.
	SessionIssued   bool `json:"sessionIssued"`
	SessionRequired bool `json:"sessionRequired"`

	// Streams (B4, D2): the new listen RPC, and what a plain GET returns.
	// "legacy-sse" means the pre-Streamable-HTTP endpoint event was seen.
	SubscriptionsListen bool   `json:"subscriptionsListen"`
	GetStream           string `json:"getStream,omitempty"` // legacy-sse|sse|405|other

	// Capability-derived (B6, D1).
	TasksExperimental bool `json:"tasksExperimental"`
	LoggingDeclared   bool `json:"loggingDeclared"`

	// tools/list result conformance (B8, B11). Advisory, never verdict-blocking.
	ResultTypePresent bool `json:"resultTypePresent"`
	CacheMetaPresent  bool `json:"cacheMetaPresent"`

	// Routing headers (B10, B13). The enforced values record which numeric
	// code came back, distinguishing RC-beta builds from final builds.
	HeaderEnforcement string `json:"headerEnforcement,omitempty"` // enforced-32020|enforced-32001|ignored|unknown

	// Resource error code migration (B12). 0 when not tested (no resources
	// capability declared).
	ResourceNotFoundCode int `json:"resourceNotFoundCode,omitempty"`

	// Auth metadata (A1): public RFC 9728 protected-resource metadata.
	ProtectedResourceMetadata bool `json:"protectedResourceMetadata"`

	// Declared layer (D2): the transport the registry declares for this
	// remote, verdict-bearing when it is the deprecated "sse".
	DeclaredTransport string `json:"declaredTransport,omitempty"`

	Verdict  string   `json:"verdict"`
	Reasons  []string `json:"reasons,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// checkReadiness runs the readiness pass against one conformant remote. base
// is the RemoteSignal the initialize probe produced for the same URL; its
// evidence (negotiated version, capabilities, session issuance) seeds the
// signal so nothing is probed twice.
func (e *Engine) checkReadiness(ctx context.Context, rm string, declaredTransport string, base RemoteSignal) *ReadinessSignal {
	sig := &ReadinessSignal{
		RulesetVersion:    RulesetVersion,
		URL:               rm,
		DeclaredTransport: declaredTransport,
		SessionIssued:     base.SessionIssued != nil && *base.SessionIssued,
	}
	for _, c := range base.Capabilities {
		switch c {
		case "tasks":
			sig.TasksExperimental = true
		case "logging":
			sig.LoggingDeclared = true
		}
	}

	// B2: a fresh tools/list with the protocol version in _meta and no prior
	// initialize. Success means the server tolerates stateless clients.
	toolsBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	stateless := e.readinessCall(ctx, rm, toolsBody, map[string]string{"Mcp-Method": "tools/list"})
	if stateless.ok {
		sig.StatelessAccepted = true
		sig.ResultTypePresent = stateless.resultType
		sig.CacheMetaPresent = stateless.cacheMeta
	}

	// B1: if stateless failed and a session was minted, retry with the
	// session header. Success proves the session mechanism carries the state
	// the server insists on: session-REQUIRED, the hard exposure.
	sessionHeaders := map[string]string{"Mcp-Method": "tools/list"}
	if !stateless.ok && sig.SessionIssued && base.sessionID != "" {
		sessionHeaders["Mcp-Session-Id"] = base.sessionID
		withSession := e.readinessCall(ctx, rm, toolsBody, sessionHeaders)
		if withSession.ok {
			sig.SessionRequired = true
			sig.ResultTypePresent = withSession.resultType
			sig.CacheMetaPresent = withSession.cacheMeta
		}
	}
	// Later calls use whichever mode worked, so a session-required server is
	// tested for headers and error codes on its own terms.
	callHeaders := map[string]string{}
	if sig.SessionRequired {
		callHeaders["Mcp-Session-Id"] = base.sessionID
	}

	// B3: server/discover, the RPC 2026-07-28 makes mandatory.
	disc := e.readinessCall(ctx, rm, `{"jsonrpc":"2.0","id":3,"method":"server/discover","params":{}}`,
		withMethod(callHeaders, "server/discover"))
	if disc.ok {
		sig.DiscoverSupported = true
		sig.DiscoverVersions = disc.versions
	}

	// B10/B13: a tools/list POST whose Mcp-Method header deliberately names a
	// different method. A conformant server rejects the mismatch; the numeric
	// code tells RC builds (-32001) from final builds (-32020).
	hdr := e.readinessCall(ctx, rm, toolsBody, withMethod(callHeaders, "resources/list"))
	switch {
	case hdr.errCode == -32020:
		sig.HeaderEnforcement = "enforced-32020"
	case hdr.errCode == -32001:
		sig.HeaderEnforcement = "enforced-32001"
	case hdr.ok:
		sig.HeaderEnforcement = "ignored"
	default:
		sig.HeaderEnforcement = "unknown"
	}

	// B4: does subscriptions/listen exist. Only a genuine result counts:
	// error codes are ambiguous across SDKs (some answer -32602 for unknown
	// methods, verified live against deepwiki), and this weak signal is not
	// verdict-bearing, so precision beats recall. The body cap keeps a real
	// stream from hanging the pass.
	sub := e.readinessCall(ctx, rm, `{"jsonrpc":"2.0","id":5,"method":"subscriptions/listen","params":{}}`,
		withMethod(callHeaders, "subscriptions/listen"))
	sig.SubscriptionsListen = sub.ok

	// D2/B4: what a plain GET on the endpoint does. The legacy HTTP+SSE
	// transport announces itself with an "endpoint" event; a modern GET
	// stream is plain SSE; a stateless-core server answers 405.
	sig.GetStream = e.getStreamBehavior(ctx, rm)

	// B12: only when the server declares resources, read a sentinel URI that
	// cannot exist and record which not-found code comes back.
	for _, c := range base.Capabilities {
		if c == "resources" {
			nf := e.readinessCall(ctx, rm,
				`{"jsonrpc":"2.0","id":6,"method":"resources/read","params":{"uri":"akashi://readiness-sentinel/does-not-exist"}}`,
				withMethod(callHeaders, "resources/read"))
			if nf.errCode == -32002 || nf.errCode == -32602 {
				sig.ResourceNotFoundCode = nf.errCode
			}
			break
		}
	}

	// A1: public RFC 9728 metadata at the origin's well-known path.
	sig.ProtectedResourceMetadata = e.protectedResourceMetadata(ctx, rm)

	classifyReadiness(sig, base)
	return sig
}

// classifyReadiness derives the verdict, reasons, and advisory warnings from
// the gathered evidence. Rules and their order come from the ruleset:
// at-risk (removed or deprecated surface) beats ready beats needs-migration.
func classifyReadiness(sig *ReadinessSignal, base RemoteSignal) {
	switch {
	case sig.DeclaredTransport == "sse" || sig.GetStream == "legacy-sse":
		sig.Verdict = ReadinessAtRisk
		sig.Reasons = append(sig.Reasons, "deprecated HTTP+SSE transport")
	case sig.TasksExperimental:
		sig.Verdict = ReadinessAtRisk
		sig.Reasons = append(sig.Reasons, "built on the removed experimental Tasks API")
	case sig.SessionRequired:
		sig.Verdict = ReadinessAtRisk
		sig.Reasons = append(sig.Reasons, "requires the removed Mcp-Session-Id session mechanism")
	case sig.DiscoverSupported && sig.StatelessAccepted && !sig.SessionIssued &&
		strings.HasPrefix(sig.HeaderEnforcement, "enforced"):
		sig.Verdict = ReadinessReady
	default:
		sig.Verdict = ReadinessNeedsMigration
		if !sig.DiscoverSupported {
			sig.Reasons = append(sig.Reasons, "no server/discover")
		}
		if !sig.StatelessAccepted {
			sig.Reasons = append(sig.Reasons, "rejects handshake-free requests")
		}
		if sig.SessionIssued {
			sig.Reasons = append(sig.Reasons, "still mints Mcp-Session-Id")
		}
		if !strings.HasPrefix(sig.HeaderEnforcement, "enforced") {
			sig.Reasons = append(sig.Reasons, "routing headers not enforced")
		}
	}

	if !sig.ResultTypePresent && (sig.StatelessAccepted || sig.SessionRequired) {
		sig.Warnings = append(sig.Warnings, "no resultType on results")
	}
	if !sig.CacheMetaPresent && (sig.StatelessAccepted || sig.SessionRequired) {
		sig.Warnings = append(sig.Warnings, "no cache metadata on list results")
	}
	if sig.ResourceNotFoundCode == -32002 {
		sig.Warnings = append(sig.Warnings, "old -32002 resource-not-found code")
	}
	if sig.LoggingDeclared {
		sig.Warnings = append(sig.Warnings, "declares the deprecated logging capability")
	}
	if base.ProtocolVersion != "" && base.ProtocolVersion < "2026-07-28" {
		sig.Warnings = append(sig.Warnings, fmt.Sprintf("negotiates %s", base.ProtocolVersion))
	}
}

// rpcOutcome condenses one readiness JSON-RPC exchange.
type rpcOutcome struct {
	status     int
	ok         bool // 2xx with a JSON-RPC result
	errCode    int  // JSON-RPC error code, 0 when none decoded
	resultType bool // result carried resultType
	cacheMeta  bool // result carried both ttlMs and cacheScope
	versions   []string
}

// readinessCall POSTs one JSON-RPC body and decodes just enough of the
// response (SSE-aware) to classify it.
func (e *Engine) readinessCall(ctx context.Context, u, body string, headers map[string]string) rpcOutcome {
	h := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json, text/event-stream",
	}
	for k, v := range headers {
		h[k] = v
	}
	r := e.fetch(ctx, http.MethodPost, u, h, []byte(body), readinessCallTimeout, 64<<10)
	out := rpcOutcome{status: r.status}
	if r.err != nil || len(r.body) == 0 {
		return out
	}
	payload := jsonRPCPayload(r.body)
	if payload == nil {
		return out
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return out
	}
	if envelope.Error != nil {
		out.errCode = envelope.Error.Code
		return out
	}
	if r.status < 200 || r.status >= 300 || len(envelope.Result) == 0 {
		return out
	}
	out.ok = true
	var res map[string]json.RawMessage
	if json.Unmarshal(envelope.Result, &res) == nil {
		_, out.resultType = res["resultType"]
		_, hasTTL := res["ttlMs"]
		_, hasScope := res["cacheScope"]
		out.cacheMeta = hasTTL && hasScope
		for _, key := range []string{"protocolVersions", "supportedVersions", "versions"} {
			if raw, found := res[key]; found {
				var vs []string
				if json.Unmarshal(raw, &vs) == nil && len(vs) > 0 {
					sort.Strings(vs)
					out.versions = vs
					break
				}
			}
		}
	}
	return out
}

// getStreamBehavior classifies what a plain GET on the endpoint returns:
// the legacy HTTP+SSE transport's "endpoint" event, a modern SSE stream, a
// stateless 405, or anything else. Reads a small prefix so an infinite
// stream cannot hang the pass.
func (e *Engine) getStreamBehavior(ctx context.Context, u string) string {
	r := e.fetch(ctx, http.MethodGet, u, map[string]string{"Accept": "text/event-stream"}, nil, readinessCallTimeout, 2<<10)
	switch {
	case r.err != nil:
		return "other"
	case r.status == http.StatusMethodNotAllowed:
		return "405"
	case r.status >= 200 && r.status < 300 && looksLikeSSE(r):
		if bytes.Contains(r.body, []byte("event: endpoint")) || bytes.Contains(r.body, []byte("event:endpoint")) {
			return "legacy-sse"
		}
		return "sse"
	default:
		return "other"
	}
}

func looksLikeSSE(r httpResult) bool {
	if r.header != nil && strings.Contains(r.header.Get("Content-Type"), "text/event-stream") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(r.body), []byte("event:")) ||
		bytes.HasPrefix(bytes.TrimSpace(r.body), []byte("data:"))
}

// protectedResourceMetadata reports whether the origin serves valid JSON at
// the RFC 9728 well-known path. Public metadata by definition; keyless.
func (e *Engine) protectedResourceMetadata(ctx context.Context, remote string) bool {
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return false
	}
	wellKnown := u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	r := e.fetch(ctx, http.MethodGet, wellKnown, jsonHeaders(), nil, readinessCallTimeout, 64<<10)
	if r.err != nil || r.status != http.StatusOK {
		return false
	}
	var v map[string]any
	return json.Unmarshal(bytes.TrimSpace(r.body), &v) == nil && len(v) > 0
}

// jsonRPCPayload returns the JSON body of a response that is either plain
// JSON or an SSE stream whose data: line carries the JSON.
func jsonRPCPayload(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		return trimmed
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			if d := strings.TrimSpace(data); strings.HasPrefix(d, "{") {
				return []byte(d)
			}
		}
	}
	return nil
}

// withMethod returns a copy of h with the Mcp-Method routing header set,
// leaving h untouched so shared base headers are never mutated between calls.
func withMethod(h map[string]string, method string) map[string]string {
	out := make(map[string]string, len(h)+1)
	for key, val := range h {
		out[key] = val
	}
	out["Mcp-Method"] = method
	return out
}
