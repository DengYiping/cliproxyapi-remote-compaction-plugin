package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

type fixtureProxy struct {
	url      string
	cmd      *exec.Cmd
	exited   chan error
	keyPath  string
	stopOnce sync.Once
}

func startProxy(t *testing.T, dir, upstream, upstreamKey, model string) *fixtureProxy {
	return startProxyKind(t, dir, upstream, upstreamKey, model, "responses")
}

func startProxyKind(t *testing.T, dir, upstream, upstreamKey, model, kind string) *fixtureProxy {
	t.Helper()
	binary, library := os.Getenv("CPA_BINARY"), os.Getenv("PLUGIN_LIBRARY")
	if binary == "" || library == "" {
		t.Skip("set CPA_BINARY and PLUGIN_LIBRARY for native integration tests")
	}
	if dir == "" {
		dir = t.TempDir()
	}
	plugins := filepath.Join(dir, "plugins", "darwin", "arm64")
	if err := os.MkdirAll(plugins, 0700); err != nil {
		t.Fatal(err)
	}
	lib, err := os.ReadFile(library)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "remote-compaction-v"+version+".dylib"), lib, 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "capsule.key")
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		if err := generateKey(keyPath); err != nil {
			t.Fatal(err)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	config := map[string]any{
		"host": "127.0.0.1", "port": port, "auth-dir": filepath.Join(dir, "auth"), "api-keys": []string{"fixture-client"},
		"request-log": false, "usage-statistics-enabled": false,
		"remote-management": map[string]any{"disable-control-panel": true},
		"plugins":           map[string]any{"enabled": true, "dir": filepath.Join(dir, "plugins"), "configs": map[string]any{pluginID: map[string]any{"enabled": true, "priority": 100, "key_file": keyPath, "models": []string{model}, "max_output_tokens": 1024}}},
		"codex-api-key":     []any{map[string]any{"api-key": upstreamKey, "base-url": upstream, "websockets": false, "models": []any{map[string]any{"name": model, "alias": model}, map[string]any{"name": "gpt-fixture", "alias": "gpt-fixture"}}}},
	}
	switch kind {
	case "chat":
		delete(config, "codex-api-key")
		config["openai-compatibility"] = []any{map[string]any{"name": "fixture-upstream", "base-url": upstream, "api-key-entries": []any{map[string]any{"api-key": upstreamKey}}, "models": []any{map[string]any{"name": model, "alias": model}}}}
	case "claude":
		delete(config, "codex-api-key")
		config["claude-api-key"] = []any{map[string]any{"api-key": upstreamKey, "base-url": strings.TrimSuffix(upstream, "/v1"), "models": []any{map[string]any{"name": model, "alias": model}}}}
	}
	raw, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", path, "--local-model")
	cmd.Dir = dir
	// Config and logs can contain credentials. Never include them in test output.
	log, err := os.OpenFile(filepath.Join(dir, "proxy.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &fixtureProxy{url: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd, exited: make(chan error, 1), keyPath: keyPath}
	go func() { p.exited <- cmd.Wait(); _ = log.Close() }()
	t.Cleanup(func() { p.stop() })
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		b, _ := os.ReadFile(filepath.Join(dir, "proxy.log"))
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "pluginhost:") && strings.Contains(line, pluginID) {
				t.Log(line)
			}
		}
	})
	client := http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", p.url+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer fixture-client")
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		select {
		case <-p.exited:
			t.Fatal("isolated CPA exited; inspect its private temporary log")
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("isolated CPA did not become ready")
	return nil
}
func (p *fixtureProxy) stop() {
	p.stopOnce.Do(func() {
		_ = p.cmd.Process.Kill()
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
		}
	})
}
func postResponse(t *testing.T, p *fixtureProxy, body []byte) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", p.url+"/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixture-client")
	req.Header.Set("Content-Type", "application/json")
	c := http.Client{Timeout: 120 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}
func responseItem(t *testing.T, raw []byte) object {
	t.Helper()
	var root object
	if json.Unmarshal(raw, &root) != nil {
		t.Fatal("invalid response JSON")
	}
	var items []object
	_ = json.Unmarshal(root["output"], &items)
	if len(items) != 1 || stringField(items[0], "type") != "compaction" {
		t.Fatalf("missing compaction output (status=%s)", stringField(root, "status"))
	}
	return items[0]
}
func wsRequest(t *testing.T, conn *websocket.Conn, body any) (object, object) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	if err := conn.WriteJSON(body); err != nil {
		t.Fatal(err)
	}
	var done object
	for i := 0; i < 1000; i++ {
		var e object
		if err := conn.ReadJSON(&e); err != nil {
			t.Fatal(err)
		}
		switch stringField(e, "type") {
		case "response.output_item.done":
			_ = json.Unmarshal(e["item"], &done)
		case "response.completed":
			var r object
			_ = json.Unmarshal(e["response"], &r)
			return r, done
		case "error", "response.failed":
			t.Fatal("WebSocket request failed")
		}
	}
	t.Fatal("too many websocket events")
	return nil, nil
}

func TestNativeProxyCompactionAndReplay(t *testing.T) {
	var mu sync.Mutex
	var seen [][]byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, b)
		mu.Unlock()
		text := "ack ORCHID"
		if bytes.Contains(b, []byte("context-compaction operation")) {
			text = "Project ORCHID; port 9443; tests green."
		}
		response := summaryResponse(text)
		var q object
		_ = json.Unmarshal(b, &q)
		if string(q["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			var root object
			_ = json.Unmarshal(response, &root)
			var items []object
			_ = json.Unmarshal(root["output"], &items)
			for _, e := range []any{map[string]any{"type": "response.created", "response": map[string]any{"id": "upstream-id", "status": "in_progress", "output": []any{}}}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": items[0]}, map[string]any{"type": "response.completed", "response": root}} {
				fmt.Fprintf(w, "data: %s\n\n", encode(e))
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(response)
		}
	}))
	defer backend.Close()
	p := startProxy(t, "", backend.URL+"/v1", "fixture-upstream", "glm-5.3-cyber")
	// Native HTTP: summary converted, then capsule expanded before ordinary generation.
	status, raw := postResponse(t, p, requestBody())
	if status != 200 {
		t.Fatalf("HTTP compaction status=%d", status)
	}
	item := responseItem(t, raw)
	r, err := newRuntime(config{KeyFile: p.keyPath, Models: []string{"glm-5.3-cyber"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.unseal("glm-5.3-cyber", stringField(item, "encrypted_content")); err != nil {
		t.Fatal(err)
	}
	status, _ = postResponse(t, p, encode(map[string]any{"model": "glm-5.3-cyber", "input": []any{item, map[string]any{"role": "user", "content": "Recall the project and port."}}}))
	if status != 200 {
		t.Fatalf("replay status=%d", status)
	}
	mu.Lock()
	if len(seen) != 2 {
		t.Errorf("expected one upstream request per client request, got %d", len(seen))
	}
	if bytes.Contains(seen[1], []byte(capsulePrefix)) || !bytes.Contains(seen[1], []byte("9443")) {
		t.Error("capsule was not expanded")
	}
	mu.Unlock()
	// Actual WebSocket: send only the delta for compaction; the proxy must reconstruct history.
	h := http.Header{"Authorization": []string{"Bearer fixture-client"}}
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(p.url, "http:", "ws:", 1)+"/v1/responses", h)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	first, _ := wsRequest(t, conn, map[string]any{"type": "response.create", "model": "glm-5.3-cyber", "input": []any{map[string]any{"role": "user", "content": "Remember project ORCHID; port 9443."}}})
	compacted, done := wsRequest(t, conn, map[string]any{"type": "response.create", "model": "glm-5.3-cyber", "previous_response_id": stringField(first, "id"), "input": []any{map[string]any{"type": "compaction_trigger"}}})
	if stringField(done, "type") != "compaction" {
		t.Fatal("Codex-required OutputItemDone is not compaction")
	}
	item = responseItem(t, encode(compacted))
	_, _ = wsRequest(t, conn, map[string]any{"type": "response.create", "model": "glm-5.3-cyber", "input": []any{item, map[string]any{"role": "user", "content": "What is the port?"}}})
	mu.Lock()
	n := len(seen)
	if n != 5 {
		t.Errorf("expected five upstream calls, got %d", n)
	}
	if n >= 5 {
		if !bytes.Contains(seen[3], []byte("Remember project ORCHID")) || !bytes.Contains(seen[3], []byte("ack ORCHID")) {
			t.Error("incremental compaction lost reconstructed history")
		}
		if bytes.Contains(seen[4], []byte(capsulePrefix)) || !bytes.Contains(seen[4], []byte("9443")) {
			t.Error("websocket replay failed")
		}
	}
	mu.Unlock()
	// Restart the isolated proxy with the same key; a capsule from the old process still works.
	dir := filepath.Dir(p.keyPath)
	p.stop()
	p2 := startProxy(t, dir, backend.URL+"/v1", "fixture-upstream", "glm-5.3-cyber")
	status, _ = postResponse(t, p2, encode(map[string]any{"model": "glm-5.3-cyber", "input": []any{item, map[string]any{"role": "user", "content": "Recall the port after restart."}}}))
	if status != 200 {
		t.Fatal("restart replay failed")
	}
	// Bad capsule must fail closed and never reach the upstream.
	mu.Lock()
	beforeCount := len(seen)
	mu.Unlock()
	status, _ = postResponse(t, p2, []byte(`{"model":"glm-5.3-cyber","input":[{"type":"compaction","encrypted_content":"tampered"}]}`))
	if status != 400 {
		t.Errorf("bad capsule status=%d", status)
	}
	mu.Lock()
	if len(seen) != beforeCount {
		t.Error("invalid capsule reached upstream")
	}
	mu.Unlock()
	status, raw = postResponse(t, p2, []byte(`{"model":"gpt-fixture","input":[{"role":"user","content":"test"},{"type":"compaction_trigger"}]}`))
	if status != 200 || bytes.Contains(raw, []byte(capsulePrefix)) {
		t.Error("GPT route was modified")
	}
	mu.Lock()
	if !bytes.Contains(seen[len(seen)-1], []byte("compaction_trigger")) {
		t.Error("GPT native compaction trigger was intercepted")
	}
	mu.Unlock()
	t.Log("native dylib: HTTP + incremental WebSocket compaction + replay + proxy restart + fail-closed capsule validation passed")
}

func TestNativeProtocolTranslators(t *testing.T) {
	for _, kind := range []string{"chat", "claude"} {
		t.Run(kind, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req object
				_ = json.Unmarshal(body, &req)
				if !bytes.Contains(body, []byte("context-compaction operation")) {
					t.Error("summary directive did not reach translated upstream")
				}
				text := "Project ORCHID; port 9443."
				if kind == "chat" {
					choice := map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}
					if string(req["stream"]) != "true" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(encode(map[string]any{"id": "chat-fixture", "object": "chat.completion", "created": 1, "model": "glm-5.3-cyber", "choices": []any{choice}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}}))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, c := range []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": text}}, map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}} {
						fmt.Fprintf(w, "data: %s\n\n", encode(map[string]any{"id": "chat-fixture", "object": "chat.completion.chunk", "created": 1, "model": "glm-5.3-cyber", "choices": []any{c}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}}))
					}
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
				message := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "claude-opus-5-5", "content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}}
				if string(req["stream"]) != "true" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(encode(message))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				start := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "claude-opus-5-5", "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 100, "output_tokens": 0}}
				for _, event := range []map[string]any{{"type": "message_start", "message": start}, {"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}, {"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}, {"type": "content_block_stop", "index": 0}, {"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 20}}, {"type": "message_stop"}} {
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encode(event))
				}
			}))
			defer backend.Close()
			model := "glm-5.3-cyber"
			if kind == "claude" {
				model = "claude-opus-5-5"
			}
			p := startProxyKind(t, "", backend.URL+"/v1", "fixture-upstream", model, kind)
			for _, streaming := range []bool{false, true} {
				b := bytes.ReplaceAll(requestBody(), []byte("glm-5.3-cyber"), []byte(model))
				var request object
				_ = json.Unmarshal(b, &request)
				request["stream"] = encode(streaming)
				status, raw := postResponse(t, p, encode(request))
				if status != 200 {
					t.Fatalf("translated status=%d", status)
				}
				if streaming {
					if !bytes.Contains(raw, []byte(`"type":"compaction"`)) || bytes.Contains(raw, []byte("response.failed")) {
						t.Fatal("translated stream did not compact")
					}
				} else {
					_ = responseItem(t, raw)
				}
			}
		})
	}
}

func TestLiveModelCompaction(t *testing.T) {
	model := os.Getenv("LIVE_MODEL")
	if model == "" {
		t.Skip("set LIVE_MODEL for an explicitly requested small live verification")
	}
	keyPath := os.Getenv("LIVE_PROXY_KEY_FILE")
	if keyPath == "" {
		t.Fatal("LIVE_PROXY_KEY_FILE is required")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal("cannot read proxy client key")
	}
	p := startProxy(t, "", "http://127.0.0.1:8317/v1", strings.TrimSpace(string(key)), model)
	marker := "ORCHID-" + uniqueID("")[:12]
	body := encode(map[string]any{"model": model, "input": []any{map[string]any{"role": "user", "content": "Project verification marker is " + marker + ". Its port is 9443. Preserve both exactly in a continuation summary."}, map[string]any{"role": "assistant", "content": "Acknowledged."}, map[string]any{"type": "compaction_trigger"}}, "stream": false})
	status, raw := postResponse(t, p, body)
	if status != 200 {
		t.Fatalf("live compaction status=%d (response not logged)", status)
	}
	item := responseItem(t, raw)
	r, err := newRuntime(config{KeyFile: p.keyPath, Models: []string{model}})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := r.unseal(model, stringField(item, "encrypted_content"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, marker) || !strings.Contains(summary, "9443") {
		t.Fatal("live summary lost the verification facts")
	}
	status, raw = postResponse(t, p, encode(map[string]any{"model": model, "input": []any{item, map[string]any{"role": "user", "content": "Reply with only the project verification marker and its port from the earlier context."}}, "max_output_tokens": 256}))
	if status != 200 {
		t.Fatalf("live replay status=%d", status)
	}
	if !bytes.Contains(raw, []byte(marker)) || !bytes.Contains(raw, []byte("9443")) {
		t.Fatal("live replay did not recall facts")
	}
	t.Logf("%s: real summary and follow-up recall passed (two small upstream requests)", model)
}
