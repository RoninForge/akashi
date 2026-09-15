# Changelog

All notable changes to akashi are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims
to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-09-15

### Added

- `akashi scan` writes the census as `records/<NN>.jsonl`, 64 shards keyed on
  a FNV-1a hash of the server's namespace, plus `records/manifest.json` with
  per-shard counts and a namespace-to-shard map. The single `records.jsonl`
  reached 51.5 MiB at 31,538 servers, past the 50 MiB GitHub warns at, and at
  2,198 bytes per server the 100 MiB hard push limit lands near 47,700: the
  census that crossed it could not have been pushed at all, discovered at
  publish time with the scan already spent. Re-sharding the real 2026-09-13
  census gives a largest shard of 5.9 MiB. Keying on the namespace rather than
  the record means a namespace is never split, so one operator's servers are
  always in exactly one file and can be fetched without the whole census.
  Nothing about a record changed, only which file it sits in.

- `akashi scan --compare <previous census>` runs a second pass after the
  census: it compares each namespace's aggregate signal rates against that
  edition and re-probes every namespace whose rates moved by at least
  `--reprobe-threshold` points (default 10), writing each second reading to
  `reprobe.jsonl` and a report into `summary.json`. A scan long enough for
  targets to change state records a transient outage as a property of the
  things measured: the 2026-09-09 census ran 16h59m and one operator's
  endpoints flipped 1,277 stateless-true readings to 270 while its health
  verdicts barely moved, which took a human to notice after publication. This
  pass is what notices. It never edits `records.jsonl`, which stays the census
  exactly as first observed; the second reading is evidence recorded beside it.
  `--reprobe-min-servers` (default 25) keeps small namespaces out, where one
  server blinking is a large percentage, and `--reprobe-max-servers`
  (default 3000) bounds the pass's wall time, recording `truncated` in the
  report rather than silently reading as a complete all-clear.

### Changed

- Censuses written before sharding keep their single `records.jsonl` and are
  still read: `scan` resolves shards first and falls back, so resume, and
  `--compare` against any historical edition, keep working. `--compare` takes
  a census directory, a `records/` directory, or a single `records.jsonl`.
  Verified by running the published build's vendor step against the real
  2026-09-13 census in both layouts: byte-identical output from each.

### Fixed

- The `--compare` entry above was filed under 0.4.0 when it merged, but
  v0.4.0 was tagged 2026-08-06 and does not contain it. Moved to the release
  that actually ships it.

## [0.4.0] - 2026-08-06

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
  output. The health verdict is unchanged and stays a pure liveness measure.
- `CITATION.cff`, so GitHub renders a "Cite this repository" button and
  citation tooling can resolve the project without scraping the README.
- The remote `initialize` probe now records its raw evidence when the
  handshake is conformant: the protocol version the server answered with, the
  sorted top-level server capability keys, and whether an `Mcp-Session-Id`
  header was issued (stateful-session usage). The fields appear in
  `akashi check --json` and in `akashi scan` records as `protocolVersion`,
  `capabilities`, and `sessionIssued`; they are the observables the
  readiness classification is computed from. The probe records evidence
  first and judges only in the classification layer.

### Changed

- The 2026-07-28 readiness ruleset is final, not a release candidate. The
  encoded rules were re-diffed against the published final specification on
  2026-08-03 and every verdict-bearing rule is unchanged from the RC: all
  eighteen breaking changes and deprecations appear with matching SEP or PR
  numbers, and the error renumbering landed as anticipated. New scans stamp
  `2026-07-28`; censuses already stamped `2026-07-28-rc` keep that stamp,
  because it names the revision that actually computed them. Acceptance of
  both the RC and the final error codes stays, deliberately: the 2026-08-03
  census found 37 servers still returning the RC `-32001` against 45 on the
  final `-32020`, so narrowing to the final numbers would silently reclassify
  a real, enforcing cohort as not enforcing at all.

### Fixed

- Every probe request is now bounded by its own timeout, and a resumed census
  keeps its real start time instead of resetting it.

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
