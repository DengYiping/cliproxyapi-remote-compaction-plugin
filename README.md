# CLIProxyAPI remote-compaction plugin

A local native C-ABI plugin that implements Codex **Responses compaction V2**
for explicitly selected non-GPT models. Each chat stays on one model. GPT
requests and native Anthropic requests are left alone.

Source and release artifacts are published under
`DengYiping/cliproxyapi-remote-compaction-plugin`.

## Design

```text
Codex -> CLIProxyAPI's normal transcript reconstruction
      -> request hook: compaction_trigger becomes a summary request
      -> existing provider, credentials, translators and generation
      -> response hook: summary becomes an authenticated/encrypted compaction item

Next request -> request hook expands the capsule -> same normal provider
```

There is **one upstream generation per compaction**, not an extra nested call.
The normal executor records usage before response interception; the plugin
preserves its usage object. It does not create an executor, change routes,
select credentials, or persist transcripts.

Summary capsules use AES-256-GCM with fresh random nonces and are bound to the
client model name. They contain the summary, format version and creation time.
The user-generated key is mandatory: there is no embedded/default encryption key.
The capsule travels in the Codex conversation, so it survives a reconnect or
proxy restart as long as the key is preserved.

## Build and checks

Go 1.26+ and a C compiler are required. The supplied Makefile targets macOS.

```sh
make build
make test
make integration
```

Outputs:

- `dist/remote-compaction`: key-generation utility.
- `dist/remote-compaction-v0.1.1.dylib`: native plugin.

For Linux, build the same package with `go build -buildmode=c-shared` and a
`.so` output name; Linux has not been verified here.

## Configure (not automatically installed)

Create a key **once**, in a directory owned by you. The command refuses to
overwrite an existing file and writes mode 0600. Do not commit the key.

```sh
./dist/remote-compaction keygen "$HOME/.cli-proxy-api/remote-compaction.key"
```

Copy the dylib into CLIProxyAPI's configured plugin directory under
`darwin/arm64/`, retaining its versioned filename. Add this to the existing
`plugins.configs` mapping in the proxy configuration:

```yaml
remote-compaction:
  enabled: true
  priority: 100
  key_file: /absolute/path/to/remote-compaction.key
  max_output_tokens: 4096
  models:
    - glm-5.3-cyber
    - glm-5.3-flash-cyber
    - glm-5.3
    - kimi-k3
    - claude-opus-5-5
    - claude-sonnet-5-5
```

Use exact **client-facing** names. Empty allowlists, wildcards, and `gpt-` names
are rejected. Add only models that your normal proxy routing can already serve.
Replace the sample `key_file` with your absolute path; YAML does not expand `$HOME`.
Priority 100 puts summary preparation ahead of existing compatibility hooks;
those hooks and routing plugins remain enabled.

The existing host supports hot-loading versioned plugin libraries and config
reloads. Confirm the plugin is registered and effectively enabled in management
before testing. No Codex catalog/provider change is required if that provider
already advertises remote compaction V2. Neither your running proxy config nor
your Codex config is changed by building or running this repository's tests.

## Verification

The native integration suite starts disposable proxy processes with temporary
configs, credentials and plugin directories. It never installs into the shared
proxy. `CPA_BINARY` defaults to nothing: tests skip unless explicitly enabled.

```sh
CPA_BINARY=/opt/homebrew/bin/cliproxyapi \
PLUGIN_LIBRARY="$PWD/dist/remote-compaction-v0.1.1.dylib" \
go test -v -run 'TestNative' .
```

To run the **Desktop-bundled Codex app-server** against a synthetic local
upstream, with subprocess-only provider overrides and an ephemeral thread:

```sh
CPA_BINARY=/opt/homebrew/bin/cliproxyapi \
PLUGIN_LIBRARY="$PWD/dist/remote-compaction-v0.1.1.dylib" \
CODEX_BINARY=/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex \
go test -v -run '^TestCodexAppServerRemoteCompaction$' .
```

An optional live test sends **two small requests**, via an isolated proxy into
your existing proxy: summarize a random verification marker, then recall it from
only the capsule. It reads the existing client key without printing it. This
test incurs normal upstream usage and does not run by default.

```sh
CPA_BINARY=/opt/homebrew/bin/cliproxyapi \
PLUGIN_LIBRARY="$PWD/dist/remote-compaction-v0.1.1.dylib" \
LIVE_MODEL=glm-5.3-cyber \
LIVE_PROXY_KEY_FILE="$HOME/.cli-proxy-api/client-key" \
go test -v -run '^TestLiveModelCompaction$' .
```

Repeat with `LIVE_MODEL=claude-opus-5-5` for the current Opus route.

## Safety and limitations

- Implements `compaction_trigger` in ordinary `/v1/responses` requests (HTTP/SSE
  and downstream WebSockets). The legacy dedicated `/responses/compact`
  endpoint is not emulated.
- Uses the reconstructed **current context**, not the complete lifetime
  transcript. Earlier truncation or summarization cannot be undone.
- Cannot decode upstream-owned native compaction capsules. Invalid, tampered,
  wrong-key and cross-model capsules fail closed before upstream execution.
  Do not switch models in an already compacted session.
- Summarization is lossy and depends on the selected model. It is not equivalent
  to native hidden-state compaction and does not guarantee every fact survives.
- Empty/incomplete/refusal/tool-call summaries do not produce successful
  capsules. Stream failures produce a failure event, never a fake completion.
- Only compaction-stream state is kept in RAM; request completion releases it.
  It is bounded to 128 concurrent compactions and 2 MiB of pending event bytes
  per request. Capsules are limited to 256 KiB of summary text.
- No prompts, summaries, keys, or capsules are logged by the plugin. CPA's own
  request logging, if enabled separately, still applies.
- Keep the same key across reloads and restarts. Losing or changing it makes old
  capsules unreadable. Back it up securely along with conversations if needed.
- Fresh conversations are the safest point to enable the plugin. Previously
  compacted conversations containing a different capsule format are unsupported.
- Reload configuration preserves immutable settings for in-flight compactions.
  The quiesce hook drains active requests before replacing the library. Do not
  forcibly unload it while a compaction is running.

See [VERIFICATION.md](VERIFICATION.md) for the checks actually performed.
