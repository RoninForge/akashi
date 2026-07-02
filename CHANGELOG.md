# Changelog

All notable changes to akashi are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims
to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
