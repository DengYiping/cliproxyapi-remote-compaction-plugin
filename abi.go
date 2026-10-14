package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* host_ctx; void* call; void* free_buffer; } cliproxy_host_api;
typedef int (*plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*plugin_free_fn)(void*, size_t);
typedef void (*plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; plugin_call_fn call; plugin_free_fn free_buffer; plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.abi_version != 1 {
		return 1
	}
	plugin.abi_version = 1
	plugin.call = C.plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, size C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if method == nil || response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	defer func() {
		if recover() != nil {
			code = writeReply(response, nil, fmt.Errorf("internal plugin failure"))
		}
	}()
	if size > maxPayloadBytes || (size > 0 && request == nil) {
		return writeReply(response, nil, badRequest("plugin request exceeds limit or is invalid"))
	}
	var raw []byte
	if size > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(size))
	}
	result, err := handle(C.GoString(method), raw)
	return writeReply(response, result, err)
}

func writeReply(out *C.cliproxy_buffer, result any, err error) C.int {
	reply := map[string]any{"ok": true, "result": result}
	var code C.int
	if err != nil {
		status := 500
		if e, ok := err.(*statusError); ok {
			status = e.status
		}
		reply = map[string]any{"ok": false, "error": map[string]any{
			"code": "remote_compaction_error", "message": err.Error(), "http_status": status,
		}}
		code = 1
	}
	encoded, errMarshal := json.Marshal(reply)
	if errMarshal != nil {
		return 1
	}
	out.ptr, out.len = C.CBytes(encoded), C.size_t(len(encoded))
	return code
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { state.quiesce() }
