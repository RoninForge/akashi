# Changelog

All notable changes to akashi are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims
to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- A 2026-07-28 spec-readiness pass. Against a server's first conformant
  remote, akashi now runs a handful of extra read-only, keyless calls (a
  handshake-free `tools/list`, `server/discover`, a routing-header mismatch
  check, a `subscriptions/listen` existence check, one GET, a sentinel
  `resources/read` when the server declares resources, and a fetch of the
  public RFC 9728 metadata) and derives a readiness verdict: `ready`,
  `needs-migration`, or `at-risk`. Servers with no keylessly reachable MCP
  endpoint get no readiness verdict at all rather than a guess. The result
  appears as a `readiness` object in `akashi check --json` and in
  `akashi scan` records, and as a `spec 2026-07-28` line in the human
  output. The ruleset is pinned as `2026-07-28-rc` and accepts both the RC
  and the renumbered final error codes; it will be re-verified against the
  final specification on its 2026-07-28 publication day. The health verdict
  is unchanged and stays a pure liveness measure.
- The remote `initialize` probe now records its raw evidence when the
  handshake is conformant: the protocol version the server answered with, the
  sorted top-level server capability keys, and whether an `Mcp-Session-Id`
  header was issued (stateful-session usage). The fields appear in
  `akashi check --json` and in `akashi scan` records as `protocolVersion`,
  `capabilities`, and `sessionIssued`; they are the observables the
  readiness classification is computed from. The probe records evidence
  first and judges only in the classification layer.

## [0.3.0] - 2026-07-02

### Added

- `akashi scan` - a bulk census command. It drains the whole official MCP
  registry and runs the exact same keyless check set as `akashi check` against
  every server, writing a dated dataset: `records.jsonl` (one probe result per
  server, byte-identical to `akashi check <server> --json`) and `summary.json`
  (verdict counts and rates, a remote-bearing segment, name-validation findings,
  and the reproducibility parameters for the run). Bounded concurrency, a
  resumable checkpoint (rerun with the same `--out` to continue an interrupted
  run), and GitHub rate-limit backoff scoped to `api.github.com`. Flags:
  `--out` (required), `--limit`, `--concurrency`, `--timeout`.
- Each probe result now carries the registry `title` and `description`, so a
  census can build per-server pages and a search index without a second lookup.
  `akashi check --json` carries them too.

### Notes

- `akashi scan` is keyless like `check`: it authenticates to no probed server
  and runs no tool. A GitHub token, if present, is used only against the public
  GitHub API to raise the rate limit.

## [0.2.0] - 2026-07-02

### Added

- **tools/list conformance probe.** After a conformant `initialize`, akashi
  opens a full MCP session with the official
  [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk)
  client and lists the server's tools. A completed session is the strongest
  keyless proof that the endpoint is a real, working MCP server. It runs no
  tool. Informational: it never downgrades the verdict (many valid servers
  advertise no tools) and is skipped for auth-gated remotes.
- **server.json schema validation.** akashi fetches the JSON Schema a registry
  server declares in its `$schema` field (cached per URL) and validates the
  published `server.json` against it. An invalid manifest downgrades the verdict
  to degraded.
- `--json` output now carries the tool count and names, and the server.json
  validation result.

## [0.1.0] - 2026-07-02

### Added

- `akashi check <server>` - keyless health probe for one MCP server, resolved
  from a registry name, a GitHub repository URL, or a remote endpoint URL.
- Health checks: registry status, repository reachability and freshness,
  package publication (npm, PyPI, Docker Hub anonymous), and remote
  reachability confirmed with a capability-only MCP `initialize` handshake.
- Conformance-lite: `initialize` handshake result, JSON-RPC id echo, and
  license presence.
- Verdict classification: healthy / degraded / dead / unknown.
- `--json` result rows and `--badge` shields.io endpoint output. A degraded,
  dead, or deprecated server never renders a green "verified" badge.
- `akashi` GitHub Action to fail CI when a server is not healthy.
