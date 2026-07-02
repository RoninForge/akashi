# akashi

Verify an MCP server is alive and conformant. Keyless.

`akashi` probes a Model Context Protocol server with only public,
unauthenticated signals and tells you whether it is healthy, degraded, or dead.
It emits an embeddable "verified on DATE" badge for the healthy ones.

The name means "proof" or "certificate" (証) in Japanese.

## Zero keys, by construction

akashi reads only public endpoints: the official MCP registry, the GitHub API,
the npm and PyPI registries, Docker Hub's anonymous manifest API, and the
server's own remote endpoint via a capability-only MCP `initialize` handshake.

It never authenticates to a probed server and never runs one of its tools. The
`initialize` request negotiates protocol capabilities and executes nothing, so
it is safe to send to a server you do not trust. Any GitHub token akashi finds
(`GITHUB_TOKEN`, `GH_TOKEN`, or your local `gh`) is used only against the public
GitHub API to raise the rate limit, exactly as a human running `gh` would. No
user secret is ever sent to a probed server.

## Install

```sh
curl -fsSL https://roninforge.org/akashi/install.sh | sh
```

Or with Go:

```sh
go install github.com/RoninForge/akashi/cmd/akashi@latest
```

## Usage

```sh
akashi check <server>
```

`<server>` may be:

- an official-registry server name (`io.github.owner/name`)
- a GitHub repository URL (`https://github.com/owner/repo`)
- a remote endpoint URL (`https://mcp.example.com/sse`)

A healthy server that ships an npm package:

```
$ akashi check ai.adeu/adeu

ai.adeu/adeu  checked 2026-07-02 UTC, keyless

  PASS  registry status                 active
  PASS  server.json valid               validates against its declared JSON Schema
  PASS  repo reachable                  exists
  PASS  repo freshness                  pushed 0d ago
  PASS  package npm                     published (1.18.1)
  PASS  at least one live entrypoint    2 alive
  PASS  license present                 MIT

  OK    healthy
```

A healthy hosted server, with its tools listed over a full MCP session:

```
$ akashi check ac.tandem/docs-mcp

ac.tandem/docs-mcp  checked 2026-07-02 UTC, keyless

  PASS  registry status                 active
  PASS  server.json valid               validates against its declared JSON Schema
  PASS  repo reachable                  exists
  PASS  remote reachable                HTTP 200 via initialize
  PASS  MCP conformance                 initialize handshake ok
  PASS  tools/list                      13 tools: search_docs, get_doc, ...
  PASS  at least one live entrypoint    2 alive

  OK    healthy
```

A dead one:

```
$ akashi check io.github.akutishevsky/spotify

io.github.akutishevsky/spotify  checked 2026-07-01 UTC, keyless

  PASS  registry status                 active
  FAIL  repo reachable                  404 (repository gone)
  FAIL  remote reachable                unreachable
  FAIL  at least one live entrypoint    nothing installable or reachable

  DEAD  dead (nothing works)
     reasons: repo_404, remote_unreachable
```

### Output formats

```sh
akashi check <server> --json     # the full result row, for scripts and CI
akashi check <server> --badge    # a shields.io endpoint badge JSON
```

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | healthy |
| 1 | degraded, dead, or unknown (a real health finding) |
| 2 | invocation or network error |

## What it checks

**Health** (can I get and run this at all):

- Registry status: the registry has not deprecated or deleted it.
- Repository reachable: the source still exists (not 404, not archived).
- Repository freshness: last push under 90 days (pass), under a year (warn),
  over a year (fail).
- Package published: the npm, PyPI, or Docker Hub entrypoint installs.
- Remote reachable: the hosted endpoint answers. akashi tries a capability-only
  MCP `initialize` handshake first (which also yields the conformance signal),
  and falls back to a plain GET for transports the handshake cannot exercise
  (for example an SSE endpoint), so a transport mismatch is never called dead.
- At least one live entrypoint: something is actually usable.

**Conformance** (is it a well-behaved MCP server):

- `server.json` validates against the JSON Schema it declares. An invalid
  manifest downgrades the verdict to degraded.
- `initialize` handshake negotiates a protocol version.
- The JSON-RPC response echoes the request id (not an HTML page impersonating a
  server with a 200).
- `tools/list` resolves over a full MCP session, using the official
  [MCP go-sdk](https://github.com/modelcontextprotocol/go-sdk) client. This is
  the strongest keyless proof that the endpoint is a real, working server (a raw
  `initialize` 200 only shows it answered one request). It runs no tool: it
  connects, reads the advertised tool list, and closes. It is informational and
  never downgrades the verdict, since many valid servers advertise no tools.
- A license is declared.

An auth-gated remote (a 401/403 to `initialize`) is treated as alive, not
broken. akashi never supplies credentials to reach past it, and does not attempt
`tools/list` against it.

## Verdicts

- **healthy** - at least one live entrypoint and nothing broken.
- **degraded** - usable, but something is broken (a 404 repo link while the
  package still installs, a stale-over-a-year repo, a deprecated registry entry,
  a down remote while a package works).
- **dead** - registry-deleted, or every probed entrypoint is broken.
- **unknown** - only entrypoints akashi cannot probe without a key were
  declared.

A degraded, dead, deprecated, or unknown server never renders a green
"verified" badge.

## Badge

`--badge` emits a [shields.io endpoint](https://shields.io/badges/endpoint-badge)
JSON. Host it and embed:

```
![MCP health](https://img.shields.io/endpoint?url=https://your.host/akashi-badge.json)
```

A healthy server reads `verified <date>` in green; anything else reads its
verdict, so a stale or broken server can never masquerade as verified.

## GitHub Action

Fail CI when a server you depend on is not healthy:

```yaml
- uses: RoninForge/akashi@v0
  with:
    server: io.github.owner/name
    # allow-degraded: true   # optional: only dead/unknown fails
```

## License

MIT. See [LICENSE](LICENSE).

Part of [RoninForge](https://roninforge.org). Sibling tools:
[hanko](https://github.com/RoninForge/hanko) (plugin manifest validator),
[tsuba](https://github.com/RoninForge/tsuba) (skill scaffolder).
