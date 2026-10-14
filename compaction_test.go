package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t testing.TB) *runtimeConfig {
	t.Helper()
	p := filepath.Join(t.TempDir(), "capsule.key")
	if err := generateKey(p); err != nil {
		t.Fatal(err)
	}
	r, err := newRuntime(config{KeyFile: p, Models: []string{"glm-5.3-cyber", "claude-opus-5-5"}, MaxOutputTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func installTestConfig(t *testing.T) *runtimeConfig {
	t.Helper()
	r := testConfig(t)
	state.Lock()
	state.config = r
	state.draining = false
	state.Unlock()
	t.Cleanup(func() {
		requests.Lock()
		ids := make([]string, 0, len(requests.items))
		for id := range requests.items {
			ids = append(ids, id)
		}
		requests.Unlock()
		for _, id := range ids {
			finishRequest(id)
		}
	})
	return r
}
func requestBody() []byte {
	return []byte(`{"model":"glm-5.3-cyber","input":[{"type":"message","role":"user","content":"Remember project ORCHID and port 9443."},{"type":"function_call","call_id":"c1","name":"read_file","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"tests green"},{"type":"compaction_trigger"}],"tools":[{"type":"function","name":"read_file"}],"previous_response_id":"old","context_management":[{"type":"compaction"}],"text":{"format":{"type":"json_object"}}}`)
}
func summaryResponse(text string) []byte {
	return encode(map[string]any{"id": "upstream-id", "object": "response", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 20, "total_tokens": 120}})
}

func TestCapsuleRoundTripAndReload(t *testing.T) {
	r := testConfig(t)
	token, err := r.seal("glm-5.3-cyber", "ORCHID 9443")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := newRuntime(r.config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := reloaded.unseal("glm-5.3-cyber", token)
	if err != nil || s != "ORCHID 9443" {
		t.Fatal("capsule reload failed")
	}
	if strings.Contains(token, "ORCHID") {
		t.Fatal("plaintext capsule")
	}
	for name, probe := range map[string]string{"foreign": "gAAAA-foreign", "truncated": capsulePrefix + "a", "modified": token[:len(token)-4] + "AAAA"} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.unseal("glm-5.3-cyber", probe); err == nil {
				t.Fatal("accepted invalid capsule")
			}
		})
	}
	if _, err := r.unseal("claude-opus-5-5", token); err == nil {
		t.Fatal("cross-model replay accepted")
	}
	other := testConfig(t)
	if _, err := other.unseal("glm-5.3-cyber", token); err == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestKeySafety(t *testing.T) {
	r := testConfig(t)
	if err := generateKey(r.KeyFile); err == nil {
		t.Fatal("overwrote existing key")
	}
	if err := os.Chmod(r.KeyFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := newRuntime(r.config); err == nil {
		t.Fatal("public key accepted")
	}
	if err := os.Chmod(r.KeyFile, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(r.KeyFile, link); err != nil {
		t.Fatal(err)
	}
	c := r.config
	c.KeyFile = link
	if _, err := newRuntime(c); err == nil {
		t.Fatal("symlink key accepted")
	}
	c = r.config
	c.Models = []string{"gpt-6.1-sol"}
	if _, err := newRuntime(c); err == nil {
		t.Fatal("GPT override accepted")
	}
}
func TestSummaryPayload(t *testing.T) {
	r := testConfig(t)
	b, err := r.summaryPayload(requestBody(), "glm-5.3-cyber")
	if err != nil {
		t.Fatal(err)
	}
	o, items, err := parseBody(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tools", "previous_response_id", "context_management", "text"} {
		if _, ok := o[field]; ok {
			t.Errorf("leaked %s", field)
		}
	}
	if len(items) != 4 || bytes.Contains(b, []byte(`"compaction_trigger"`)) || !bytes.Contains(b, []byte("tests green")) {
		t.Fatal("lost history or retained trigger")
	}
	if string(o["max_output_tokens"]) != "512" || string(o["stream"]) != "false" {
		t.Fatal("summary limits missing")
	}
}
func TestSummaryRejectsInvalidOutputs(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"incomplete"}`, `{"status":"completed","output":[]}`, `{"status":"completed","output":[{"type":"function_call"}]}`, `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal"}]}]}`} {
		if _, _, err := extractSummary([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
func TestHookScopeAndNonstream(t *testing.T) {
	r := installTestConfig(t)
	for _, test := range []struct{ model, format string }{{"gpt-6.1-sol", "openai-response"}, {"glm-5.3-cyber", "claude"}, {"unlisted", "openai-response"}} {
		o, _ := before(interceptRequest{RequestID: "no-op", RequestedModel: test.model, SourceFormat: test.format, Body: requestBody()})
		if len(o.(hookResult).Body) > 0 {
			t.Fatal("changed out-of-scope request")
		}
	}
	_, err := before(interceptRequest{RequestID: "one", RequestedModel: "glm-5.3-cyber", SourceFormat: "openai-response", Body: requestBody()})
	if err != nil {
		t.Fatal(err)
	}
	o, err := after(interceptRequest{RequestID: "one", Body: summaryResponse("ORCHID port 9443; tests green")})
	if err != nil {
		t.Fatal(err)
	}
	var root object
	_ = json.Unmarshal(o.(hookResult).Body, &root)
	var items []object
	_ = json.Unmarshal(root["output"], &items)
	if len(items) != 1 || stringField(items[0], "type") != "compaction" {
		t.Fatal("not a compaction")
	}
	s, err := r.unseal("glm-5.3-cyber", stringField(items[0], "encrypted_content"))
	if err != nil || !strings.Contains(s, "9443") {
		t.Fatal("summary not preserved")
	}
	if string(root["usage"]) != string(encode(map[string]any{"input_tokens": 100, "output_tokens": 20, "total_tokens": 120})) {
		t.Fatal("usage changed")
	}
}
func TestStreamingFrames(t *testing.T) {
	for _, framing := range []string{"sse", "crlf", "json"} {
		for _, size := range []int{1, 7, 4096} {
			t.Run(fmt.Sprintf("%s/%d", framing, size), func(t *testing.T) {
				r := installTestConfig(t)
				id := t.Name()
				_, _ = before(interceptRequest{RequestID: id, RequestedModel: "glm-5.3-cyber", SourceFormat: "openai-response", Body: requestBody()})
				var resp any
				_ = json.Unmarshal(summaryResponse("ORCHID 9443"), &resp)
				event := encode(map[string]any{"type": "response.completed", "response": resp})
				data := event
				if framing != "json" {
					data = []byte("event: response.completed\ndata: " + string(event) + "\n\n")
				}
				if framing == "crlf" {
					data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
				}
				var out []byte
				for len(data) > 0 {
					n := min(size, len(data))
					v, err := stream(interceptRequest{RequestID: id, Body: data[:n]})
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, v.(hookResult).Body...)
					data = data[n:]
				}
				if bytes.Count(out, []byte(`"type":"response.completed"`)) != 1 || bytes.Contains(out, []byte(`"type":"output_text"`)) {
					t.Fatalf("invalid compaction stream: %s", out)
				}
				// Entire synthesized sequence is valid; exactly one done compaction is emitted.
				pending := out
				found := 0
				for len(pending) > 0 {
					e, rest, err := nextEvent(pending)
					if err != nil {
						t.Fatal(err)
					}
					if len(rest) == len(pending) {
						break
					}
					pending = rest
					var o object
					_ = json.Unmarshal(e, &o)
					if stringField(o, "type") == "response.output_item.done" {
						var item object
						_ = json.Unmarshal(o["item"], &item)
						if _, err := r.unseal("glm-5.3-cyber", stringField(item, "encrypted_content")); err != nil {
							t.Fatal(err)
						}
						found++
					}
				}
				if found != 1 {
					t.Fatal("expected one compaction item")
				}
			})
		}
	}
}
func TestReplayAndRecompaction(t *testing.T) {
	r := testConfig(t)
	token, _ := r.seal("glm-5.3-cyber", "ORCHID port 9443")
	b := encode(map[string]any{"input": []any{map[string]any{"type": "compaction", "encrypted_content": token}, map[string]any{"type": "compaction_trigger"}}})
	out, err := r.summaryPayload(b, "glm-5.3-cyber")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(capsulePrefix)) || !bytes.Contains(out, []byte("ORCHID port 9443")) {
		t.Fatal("capsule not expanded for repeat compaction")
	}
}
func TestQuiesceWaitsForCompletion(t *testing.T) {
	installTestConfig(t)
	_, _ = before(interceptRequest{RequestID: "drain", RequestedModel: "glm-5.3-cyber", SourceFormat: "openai-response", Body: requestBody()})
	done := make(chan struct{})
	go func() { state.quiesce(); close(done) }()
	state.Lock()
	active := state.active
	state.Unlock()
	if active == 0 {
		t.Fatal("missing active request")
	}
	finishRequest("drain")
	<-done
	if _, _, err := state.acquire(); err == nil {
		t.Fatal("accepted while draining")
	}
}
func TestFailureDoesNotProduceCapsule(t *testing.T) {
	installTestConfig(t)
	_, _ = before(interceptRequest{RequestID: "failed", RequestedModel: "glm-5.3-cyber", SourceFormat: "openai-response", Body: requestBody()})
	v, err := stream(interceptRequest{RequestID: "failed", Body: []byte("data: {\"type\":\"response.incomplete\"}\n\n")})
	if err != nil {
		t.Fatal(err)
	}
	b := v.(hookResult).Body
	if !bytes.Contains(b, []byte("response.failed")) || bytes.Contains(b, []byte(capsulePrefix)) {
		t.Fatal("failure reported success")
	}
}

func TestReconfigurePreservesActiveRequest(t *testing.T) {
	r := installTestConfig(t)
	_, _ = before(interceptRequest{RequestID: "refresh", RequestedModel: "glm-5.3-cyber", SourceFormat: "openai-response", Body: requestBody()})
	configuration := []byte(fmt.Sprintf("key_file: %q\nmodels: [glm-5.3-cyber]\nmax_output_tokens: 1024\n", r.KeyFile))
	if err := state.configure(configuration); err != nil {
		t.Fatal(err)
	}
	active := getRequest("refresh")
	if active == nil || active.config != r {
		t.Fatal("config refresh discarded in-flight context")
	}
	o, err := after(interceptRequest{RequestID: "refresh", Body: summaryResponse("ORCHID 9443")})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(o.(hookResult).Body, []byte(capsulePrefix)) {
		t.Fatal("config refresh disabled response hook")
	}
}

func TestRetryAndInterleavedRequests(t *testing.T) {
	installTestConfig(t)
	for _, id := range []string{"retry", "other"} {
		_, _ = before(interceptRequest{RequestID: id, RequestedModel: "glm-5.3-cyber", SourceFormat: "openai-response", Body: requestBody()})
	}
	_, _ = stream(interceptRequest{RequestID: "retry", Body: []byte(`{"type":"response.`)})
	_, _ = stream(interceptRequest{RequestID: "retry", ChunkIndex: -1})
	for _, id := range []string{"other", "retry"} {
		var resp any
		_ = json.Unmarshal(summaryResponse(id), &resp)
		v, err := stream(interceptRequest{RequestID: id, Body: encode(map[string]any{"type": "response.completed", "response": resp})})
		if err != nil || !bytes.Contains(v.(hookResult).Body, []byte(capsulePrefix)) {
			t.Fatal("retry or interleaving lost request state")
		}
	}
}

func FuzzCapsule(f *testing.F) {
	f.Add("garbage")
	f.Add(capsulePrefix + "abc")
	r := testConfig(f)
	f.Fuzz(func(t *testing.T, s string) { _, _ = r.unseal("glm-5.3-cyber", s) })
}
