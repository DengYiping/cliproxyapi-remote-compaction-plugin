# Changelog

## 0.1.1

First public release.

- Emulate Codex Responses V2 compaction for an explicit non-GPT model allowlist.
- Preserve existing upstream routing and token accounting.
- Support HTTP/SSE and downstream WebSocket transcript reconstruction.
- Store authenticated, model-bound summaries in self-contained AES-GCM capsules.
- Preserve active requests across configuration refreshes and drain on replacement.
- Include unit/race/fuzz coverage, native protocol integration tests, and a
  Desktop Codex app-server compatibility test.

The initial local prototype was version 0.1.0. Version 0.1.1 updates publication
metadata and portability; the compaction protocol is unchanged.
