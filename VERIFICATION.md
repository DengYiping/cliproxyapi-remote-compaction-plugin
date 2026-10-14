# Verification — 2026-10-14

The pre-installation checks used disposable proxy instances. Subsequent
installation checks also exercised the enabled plugin in the local shared proxy.
No private configuration, credentials, transcripts, or test artifacts are
included in this repository.

## Completed checks

- Go unit tests and race detector: authenticated capsule round-trip, persistence
  across reload, wrong-key/model/tamper rejection, private-key file rules,
  summary payload cleanup, exact model/protocol scoping, non-stream response
  conversion, single-byte and batched SSE/CRLF/bare-JSON framing, repeated
  compaction, draining, failures, retries and concurrent request isolation.
- Full native proxy and Desktop Codex suite passed under `go test -race`;
  `go vet` passed. Capsule fuzzing completed 559,304 executions without a failure.
- Native dylib loaded by installed CLIProxyAPI 8.0.15: HTTP compaction/replay,
  incremental downstream WebSocket compaction/replay, proxy restart with the
  same key, rejection before upstream of invalid capsules. One upstream request
  per client request; no recursive generation call.
- Native Chat Completions and Anthropic Messages translators: both non-streaming
  and SSE summary responses are converted into valid compaction results.
- Desktop-bundled Codex app-server: ephemeral thread, ordinary turn, explicit
  `thread/compact/start`, accepted `contextCompaction` completion, and another
  successful turn using the expanded summary. Upstream is synthetic for this
  protocol test, not a claim about model quality or long-context performance.
- Live `glm-5.3-cyber`, using the existing shared proxy as upstream: preserved a
  unique verification marker and port in a summary; a follow-up with only the
  capsule recalled both correctly.
- Live `claude-opus-5-5`, using the existing shared proxy as upstream: the same
  summary-and-recall test passed.

The first live GLM attempt caught a configuration-refresh bug: rejecting a
reconfigure while a compaction was active caused the host to drop the plugin's
hooks. Fixed by retaining an immutable config snapshot per active request and
allowing the host to refresh the config. A new regression test covers it; the
repeat live GLM test passed.

These checks do not certify arbitrary models, million-token conversations,
native upstream capsules, cross-model switching, or the legacy compact endpoint.
## Installation verification

The plugin was hot-loaded successfully without restarting the proxy. Live
summary-and-recall tests passed for GLM 5.3 Cyber, Opus 5.5, and Gemini 4 Argon.
A separate ephemeral Desktop-bundled Codex session using Gemini completed a
generation, remote compaction, and a subsequent turn. Ordinary Gemini tool
calls and tool-result replay also passed. These tests are bounded compatibility
checks, not a guarantee of arbitrary long-context workloads.
