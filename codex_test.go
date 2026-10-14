package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type appServer struct {
	t       *testing.T
	stdin   io.WriteCloser
	events  chan object
	backlog []object
	next    int
}

func (a *appServer) send(v any) {
	a.t.Helper()
	if _, err := fmt.Fprintln(a.stdin, string(encode(v))); err != nil {
		a.t.Fatal(err)
	}
}
func (a *appServer) wait(match func(object) bool) object {
	a.t.Helper()
	for i, e := range a.backlog {
		if match(e) {
			a.backlog = append(a.backlog[:i], a.backlog[i+1:]...)
			return e
		}
	}
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-a.events:
			if !ok {
				a.t.Fatal("Codex app-server exited before completing the verification")
			}
			if match(e) {
				return e
			}
			a.backlog = append(a.backlog, e)
		case <-timer.C:
			a.t.Fatal("Codex app-server verification timed out")
		}
	}
}
func (a *appServer) call(method string, params any) object {
	a.t.Helper()
	a.next++
	id := a.next
	a.send(map[string]any{"id": id, "method": method, "params": params})
	e := a.wait(func(e object) bool { return string(e["id"]) == fmt.Sprint(id) })
	if len(e["error"]) > 0 {
		a.t.Fatalf("Codex %s failed: %s", method, e["error"])
	}
	var out object
	if json.Unmarshal(e["result"], &out) != nil {
		a.t.Fatalf("invalid %s result", method)
	}
	return out
}

func TestCodexAppServerRemoteCompaction(t *testing.T) {
	binary := os.Getenv("CODEX_BINARY")
	if binary == "" {
		t.Skip("set CODEX_BINARY to validate the Desktop-bundled app-server")
	}
	var seen [][]byte
	var seenMu sync.Mutex
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenMu.Lock()
		seen = append(seen, body)
		seenMu.Unlock()
		text := "ACK"
		if bytes.Contains(body, []byte("context-compaction operation")) {
			text = "Project ORCHID; listening port 9443; user asked to retain these facts."
		} else if bytes.Contains(body, []byte("What is the port")) {
			text = "9443"
		}
		var response object
		_ = json.Unmarshal(summaryResponse(text), &response)
		response["id"] = encode(uniqueID("resp_fixture_"))
		var items []object
		_ = json.Unmarshal(response["output"], &items)
		items[0]["id"], items[0]["status"] = encode(uniqueID("msg_")), encode("completed")
		response["output"] = encode(items)
		var q object
		_ = json.Unmarshal(body, &q)
		if string(q["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, e := range []any{map[string]any{"type": "response.created", "response": map[string]any{"id": stringField(response, "id"), "status": "in_progress", "output": []any{}}}, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": items[0]}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": items[0]}, map[string]any{"type": "response.completed", "response": response}} {
				fmt.Fprintf(w, "data: %s\n\n", encode(e))
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(encode(response))
		}
	}))
	defer backend.Close()
	p := startProxy(t, "", backend.URL+"/v1", "fixture-upstream", "glm-5.3-cyber")
	args := []string{"-c", `features.plugins=false`, "-c", `model_providers.compactiontest.name="Compaction test"`, "-c", `model_providers.compactiontest.base_url="` + p.url + `/v1"`, "-c", `model_providers.compactiontest.wire_api="responses"`, "-c", `model_providers.compactiontest.env_key="COMPACTION_TEST_KEY"`, "-c", `model_providers.compactiontest.requires_openai_auth=false`, "-c", `model_providers.compactiontest.supports_websockets=true`, "-c", `model_providers.compactiontest.capabilities.remote_compaction="v2"`}
	// Disable only this subprocess's MCP servers; do not edit the user's configuration.
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	userConfig, err := os.ReadFile(filepath.Join(userHome, ".codex", "config.toml"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(userConfig), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[mcp_servers.") && strings.HasSuffix(line, "]") && !strings.HasSuffix(line, "]]") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "[mcp_servers."), "]")
			if !strings.Contains(name, ".") {
				args = append(args, "-c", "mcp_servers."+name+".enabled=false")
			}
		}
	}
	args = append(args, "app-server")
	cmd := exec.Command(binary, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "COMPACTION_TEST_KEY=fixture-client")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	a := &appServer{t: t, stdin: stdin, events: make(chan object, 1024)}
	go func() {
		defer close(a.events)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			var e object
			if json.Unmarshal(scanner.Bytes(), &e) == nil {
				a.events <- e
			}
		}
	}()
	a.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "compaction-plugin-test", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}})
	a.send(map[string]any{"method": "initialized", "params": map[string]any{}})
	started := a.call("thread/start", map[string]any{"model": "glm-5.3-cyber", "modelProvider": "compactiontest", "cwd": cmd.Dir, "ephemeral": true, "approvalPolicy": "never", "sandbox": "read-only", "baseInstructions": "Do not use tools. Follow the user's request and answer briefly.", "config": map[string]any{"features.plugins": false, "model_reasoning_effort": "low"}})
	var thread object
	_ = json.Unmarshal(started["thread"], &thread)
	id := stringField(thread, "id")
	if id == "" {
		t.Fatal("thread/start returned no ID")
	}
	turn := func(text string) {
		a.call("turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": text, "text_elements": []any{}}}})
		e := a.wait(func(e object) bool { return stringField(e, "method") == "turn/completed" })
		var params struct {
			Turn struct {
				Status string `json:"status"`
				Error  any    `json:"error"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(e["params"], &params)
		if params.Turn.Status != "completed" {
			t.Fatalf("Codex turn status=%s error=%v", params.Turn.Status, params.Turn.Error)
		}
	}
	turn("Remember project ORCHID and listening port 9443. Reply ACK.")
	a.call("thread/compact/start", map[string]any{"threadId": id})
	e := a.wait(func(e object) bool { return stringField(e, "method") == "turn/completed" })
	if bytes.Contains(e["params"], []byte(`"status":"failed"`)) {
		t.Fatalf("Codex compaction failed: %s", e["params"])
	}
	// The resulting thread must include a completed contextCompaction item.
	compactionEvent := a.wait(func(e object) bool {
		return stringField(e, "method") == "item/completed" && bytes.Contains(e["params"], []byte("contextCompaction"))
	})
	if len(compactionEvent) == 0 {
		t.Fatal("Codex did not accept the compaction item")
	}
	turn("What is the port from the earlier context? Reply with only the number.")
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) < 3 {
		t.Fatalf("expected generation, compaction, and continuation; got %d calls", len(seen))
	}
	if !bytes.Contains(seen[1], []byte("context-compaction operation")) {
		t.Fatal("Codex did not use remote compaction")
	}
	last := seen[len(seen)-1]
	if bytes.Contains(last, []byte(capsulePrefix)) || !bytes.Contains(last, []byte("Project ORCHID; listening port 9443")) {
		t.Fatal("Codex continuation did not use expanded summary")
	}
	t.Log("Desktop-bundled Codex accepted remote compaction and continued the same thread through the native plugin")
}
