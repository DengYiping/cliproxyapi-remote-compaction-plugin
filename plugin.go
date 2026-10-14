package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

type activeRequest struct {
	sync.Mutex
	config   *runtimeConfig
	model    string
	pending  []byte
	terminal bool
	release  func()
}

var requests = struct {
	sync.Mutex
	items map[string]*activeRequest
}{items: make(map[string]*activeRequest)}

type interceptRequest struct {
	RequestID, SourceFormat, Model, RequestedModel string
	Body                                           []byte
	ChunkIndex                                     int
}
type hookResult struct {
	Body         []byte `json:",omitempty"`
	DropChunk    bool   `json:",omitempty"`
	Terminate    bool   `json:",omitempty"`
	StatusCode   int    `json:",omitempty"`
	ResponseBody []byte `json:",omitempty"`
}

func isResponses(format string) bool {
	switch format {
	case "responses", "openai-response", "codex":
		return true
	}
	return false
}

func finishRequest(id string) {
	requests.Lock()
	r := requests.items[id]
	delete(requests.items, id)
	requests.Unlock()
	if r != nil {
		r.release()
	}
}

func handle(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if json.Unmarshal(raw, &req) != nil {
			return nil, fmt.Errorf("invalid lifecycle request")
		}
		if err := state.configure(req.ConfigYAML); err != nil {
			return nil, err
		}
		return map[string]any{
			"schema_version": 6,
			"metadata": map[string]any{"Name": pluginID, "Version": version, "Author": "DengYiping", "GitHubRepository": "https://github.com/DengYiping/cliproxyapi-remote-compaction-plugin", "ConfigFields": []any{
				map[string]any{"Name": "key_file", "Type": "string", "Description": "Absolute path to a private 32-byte AES key; keep it across reloads."},
				map[string]any{"Name": "models", "Type": "array", "Description": "Exact non-GPT client model identifiers."},
				map[string]any{"Name": "max_output_tokens", "Type": "integer", "Description": "Summary output cap, 256 to 16384 (default 4096)."},
			}},
			"capabilities": map[string]bool{"request_interceptor": true, "request_lifecycle_plugin": true, "response_interceptor": true, "response_stream_interceptor": true},
		}, nil
	case "plugin.quiesce", "plugin.shutdown":
		state.quiesce()
		return struct{}{}, nil
	case "request.complete":
		var req interceptRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, fmt.Errorf("invalid completion")
		}
		finishRequest(req.RequestID)
		return struct{}{}, nil
	case "request.intercept_before", "request.intercept_after", "response.intercept_after", "response.intercept_stream_chunk":
		var req interceptRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, fmt.Errorf("invalid interceptor request")
		}
		switch method {
		case "request.intercept_before":
			return before(req)
		case "request.intercept_after":
			return hookResult{}, nil
		case "response.intercept_after":
			return after(req)
		default:
			return stream(req)
		}
	default:
		return nil, fmt.Errorf("unsupported plugin method")
	}
}

func before(req interceptRequest) (any, error) {
	if !isResponses(req.SourceFormat) {
		return hookResult{}, nil
	}
	model := req.RequestedModel
	if model == "" {
		model = req.Model
	}
	state.Lock()
	cfg := state.config
	selected := cfg != nil && cfg.models[canonical(model)]
	state.Unlock()
	if !selected {
		return hookResult{}, nil
	}
	var body object
	if json.Unmarshal(req.Body, &body) != nil {
		return hookResult{}, nil
	}
	var items []object
	if json.Unmarshal(body["input"], &items) != nil {
		return hookResult{}, nil
	}
	trigger, compact := false, false
	for _, item := range items {
		switch stringField(item, "type") {
		case "compaction_trigger":
			trigger = true
		case "compaction":
			compact = true
		}
	}
	if !trigger && !compact {
		return hookResult{}, nil
	}
	cfg, release, err := state.acquire()
	if err != nil {
		return reject(err), nil
	}
	if !trigger {
		defer release()
		out, changed, err := cfg.expand(req.Body, model)
		if err != nil {
			return reject(err), nil
		}
		if changed {
			return hookResult{Body: out}, nil
		}
		return hookResult{}, nil
	}
	if req.RequestID == "" {
		release()
		return reject(badRequest("compaction requires a request identity")), nil
	}
	out, err := cfg.summaryPayload(req.Body, model)
	if err != nil {
		release()
		return reject(err), nil
	}
	requests.Lock()
	old := requests.items[req.RequestID]
	if old == nil && len(requests.items) >= 128 {
		requests.Unlock()
		release()
		return reject(&statusError{503, "too many concurrent compaction requests"}), nil
	}
	requests.items[req.RequestID] = &activeRequest{config: cfg, model: model, release: release}
	requests.Unlock()
	if old != nil {
		old.release()
	}
	return hookResult{Body: out}, nil
}

func reject(err error) hookResult {
	status := 502
	if e, ok := err.(*statusError); ok {
		status = e.status
	}
	return hookResult{Terminate: true, StatusCode: status, ResponseBody: encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "remote_compaction_error", "message": err.Error()}})}
}

func getRequest(id string) *activeRequest {
	requests.Lock()
	defer requests.Unlock()
	return requests.items[id]
}
func complete(r *activeRequest, body []byte) (object, error) {
	summary, usage, err := extractSummary(body)
	if err != nil {
		return nil, err
	}
	token, err := r.config.seal(r.model, summary)
	if err != nil {
		return nil, err
	}
	return compactionResponse(r.model, token, usage), nil
}

func after(req interceptRequest) (any, error) {
	r := getRequest(req.RequestID)
	if r == nil {
		return hookResult{}, nil
	}
	r.Lock()
	defer r.Unlock()
	response, err := complete(r, req.Body)
	if err != nil {
		return nil, err
	}
	r.terminal = true
	return hookResult{Body: encode(response)}, nil
}

func failedStream(message string) []byte {
	e := map[string]any{"type": "response.failed", "response": map[string]any{"id": uniqueID("resp_rc_"), "object": "response", "status": "failed", "output": []any{}, "error": map[string]string{"code": "remote_compaction_error", "message": message}}}
	return []byte(fmt.Sprintf("event: response.failed\ndata: %s\n\n", encode(e)))
}

func stream(req interceptRequest) (any, error) {
	r := getRequest(req.RequestID)
	if r == nil {
		return hookResult{}, nil
	}
	r.Lock()
	defer r.Unlock()
	if req.ChunkIndex < 0 {
		r.pending = nil
		r.terminal = false
		return hookResult{}, nil
	}
	if r.terminal {
		return hookResult{DropChunk: true}, nil
	}
	r.pending = append(r.pending, req.Body...)
	if len(r.pending) > 2<<20 {
		r.pending = nil
		r.terminal = true
		return hookResult{Body: failedStream("summary stream exceeded its buffer limit")}, nil
	}
	var result []byte
	for {
		payload, remaining, err := nextEvent(r.pending)
		if err != nil {
			r.pending = nil
			r.terminal = true
			return hookResult{Body: failedStream("invalid summary stream")}, nil
		}
		if len(remaining) == len(r.pending) {
			break
		}
		r.pending = remaining
		if len(payload) == 0 {
			continue
		}
		var e object
		if json.Unmarshal(payload, &e) != nil {
			r.terminal = true
			return hookResult{Body: failedStream("invalid summary event")}, nil
		}
		switch stringField(e, "type") {
		case "response.completed":
			response, err := complete(r, e["response"])
			r.terminal = true
			r.pending = nil
			if err != nil {
				return hookResult{Body: failedStream(err.Error())}, nil
			}
			for _, c := range compactionChunks(response) {
				result = append(result, c["Payload"]...)
			}
			return hookResult{Body: result}, nil
		case "response.failed", "response.incomplete", "error":
			r.terminal = true
			r.pending = nil
			return hookResult{Body: failedStream("upstream summary failed or was incomplete; history was not compacted")}, nil
		}
	}
	return hookResult{DropChunk: true}, nil
}

// Accept SSE (including CRLF and fragmented lines), bare JSON events, and batches.
func nextEvent(p []byte) (json.RawMessage, []byte, error) {
	t := bytes.TrimLeft(p, " \r\n\t")
	if len(t) == 0 {
		return nil, nil, nil
	}
	if t[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(t))
		var event json.RawMessage
		err := dec.Decode(&event)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, p, nil
		}
		if err != nil {
			return nil, nil, err
		}
		return event, t[dec.InputOffset():], nil
	}
	i := bytes.IndexByte(t, '\n')
	if i < 0 {
		if bytes.HasPrefix(t, []byte("data:")) && json.Valid(bytes.TrimSpace(t[5:])) {
			return bytes.TrimSpace(t[5:]), nil, nil
		}
		return nil, p, nil
	}
	line, rest := bytes.TrimSpace(t[:i]), t[i+1:]
	if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimSpace(line[5:])
		if bytes.Equal(data, []byte("[DONE]")) {
			return nil, rest, fmt.Errorf("stream ended without a compaction result")
		}
		if len(data) == 0 {
			return nil, rest, nil
		}
		if !json.Valid(data) {
			return nil, nil, fmt.Errorf("invalid SSE JSON")
		}
		return data, rest, nil
	}
	if len(line) == 0 || line[0] == ':' || bytes.HasPrefix(line, []byte("event:")) || bytes.HasPrefix(line, []byte("id:")) || bytes.HasPrefix(line, []byte("retry:")) {
		return nil, rest, nil
	}
	return nil, nil, fmt.Errorf("invalid stream framing")
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "keygen" {
		if err := generateKey(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Created private capsule key (keep it to resume compacted sessions).")
		return
	}
	fmt.Fprintln(os.Stderr, "Native CLIProxyAPI plugin. Usage: remote-compaction keygen /absolute/path/to/key")
}
