package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/RoninForge/akashi/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// toolsResult is the outcome of the go-sdk tools/list probe.
type toolsResult struct {
	status string // ok | empty | error | connect_failed
	count  int
	names  []string
}

// mcpToolsList opens a full MCP session with the official client and calls
// tools/list. A completed session that returns a tool list is the strongest
// keyless proof that the endpoint is a real, working MCP server (a raw
// initialize 200 only shows it answered one request). It runs no tool: it
// connects, reads the advertised tool list, and closes. Retries and the
// standalone server-to-client SSE stream are disabled so it is a one-shot.
func (e *Engine) mcpToolsList(ctx context.Context, endpoint string) toolsResult {
	client := mcp.NewClient(&mcp.Implementation{Name: "akashi", Version: version.Get().Version}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           e.HTTP,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}

	cctx, cancel := context.WithTimeout(ctx, e.RemoteTimeout)
	defer cancel()

	session, err := client.Connect(cctx, transport, nil)
	if err != nil {
		return toolsResult{status: "connect_failed"}
	}
	defer func() { _ = session.Close() }()

	res, err := session.ListTools(cctx, nil)
	if err != nil {
		return toolsResult{status: "error"}
	}

	out := toolsResult{count: len(res.Tools), status: "ok"}
	if len(res.Tools) == 0 {
		out.status = "empty"
	}
	for _, t := range res.Tools {
		if len(out.names) >= 8 {
			break
		}
		out.names = append(out.names, t.Name)
	}
	return out
}

// validateServerJSON validates a published server.json object against the JSON
// Schema it declares in its "$schema" field. The schema is fetched keyless and
// the compiled result is cached per URL on the Engine.
func (e *Engine) validateServerJSON(ctx context.Context, raw json.RawMessage) ServerJSONSignal {
	if len(raw) == 0 {
		return ServerJSONSignal{Status: "absent"}
	}
	var head struct {
		Schema string `json:"$schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return ServerJSONSignal{Status: "error", Errors: []string{"unreadable server.json: " + err.Error()}}
	}
	if !strings.HasPrefix(head.Schema, "http") {
		return ServerJSONSignal{Status: "no_schema"}
	}

	schema, err := e.serverSchema(ctx, head.Schema)
	if err != nil {
		// Could not fetch or compile the schema. Do not penalize the server for
		// our own inability to validate; report it as an error, not invalid.
		return ServerJSONSignal{Status: "error", Schema: head.Schema, Errors: []string{"could not load schema: " + err.Error()}}
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return ServerJSONSignal{Status: "error", Schema: head.Schema, Errors: []string{"unreadable server.json: " + err.Error()}}
	}
	if err := schema.Validate(inst); err != nil {
		return ServerJSONSignal{Status: "invalid", Schema: head.Schema, Errors: schemaErrorLines(err)}
	}
	return ServerJSONSignal{Status: "valid", Schema: head.Schema}
}

// serverSchema fetches and compiles a JSON Schema by URL, caching the compiled
// schema so a bulk scan fetches each schema version only once.
func (e *Engine) serverSchema(ctx context.Context, schemaURL string) (*jsonschema.Schema, error) {
	e.schemaMu.Lock()
	defer e.schemaMu.Unlock()
	if e.schemaCache == nil {
		e.schemaCache = make(map[string]*jsonschema.Schema)
	}
	if s, ok := e.schemaCache[schemaURL]; ok {
		return s, nil
	}

	r := e.fetch(ctx, http.MethodGet, schemaURL, jsonHeaders(), nil, e.RequestTimeout, 8<<20)
	if r.err != nil {
		return nil, r.err
	}
	if r.status != http.StatusOK {
		return nil, fmt.Errorf("schema HTTP %d", r.status)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(r.body))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(schemaURL)
	if err != nil {
		return nil, err
	}
	e.schemaCache[schemaURL] = s
	return s, nil
}

// schemaErrorLines renders a validation error as a handful of readable lines.
func schemaErrorLines(err error) []string {
	out := make([]string, 0, 5)
	for _, line := range strings.Split(strings.TrimSpace(err.Error()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) >= 5 {
			break
		}
	}
	return out
}
