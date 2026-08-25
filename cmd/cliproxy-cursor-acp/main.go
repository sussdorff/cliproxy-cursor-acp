package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static size_t bounded_method_len(const char* value) { return strnlen(value, 129); }
*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func main() {}

// cliproxy_plugin_init provides CLIProxyAPI's native C plugin ABI. The ABI
// layout follows the public, MIT-licensed reference plugin by nyanjou.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || uint32(host.abi_version) != pluginabi.ABIVersion {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeEnvelope(response, errorEnvelope("invalid_method", "method is required", false))
		return 1
	}
	if request == nil && requestLen > 0 {
		writeEnvelope(response, errorEnvelope("invalid_request", "request buffer is invalid", false))
		return 1
	}
	if uint64(requestLen) > maxABIRequestBytes {
		writeEnvelope(response, errorEnvelope("request_too_large", "request exceeds plugin limit", false))
		return 1
	}
	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	methodLength := C.bounded_method_len(method)
	if methodLength > C.size_t(maxABIMethodBytes) {
		writeEnvelope(response, errorEnvelope("invalid_method", "method is invalid", false))
		return 1
	}
	methodName := C.GoStringN(method, C.int(methodLength))
	if err := validateABIInput(methodName, uint64(len(raw))); err != nil {
		writeEnvelope(response, errorEnvelope("invalid_request", "invalid plugin request", false))
		return 1
	}
	result, failed := safeDispatch(methodName, raw)
	writeEnvelope(response, result)
	if failed {
		return 1
	}
	return 0
}

const maxABIRequestBytes uint64 = 2 << 20
const maxABIMethodBytes = 128

func validateABIInput(method string, length uint64) error {
	if len(method) == 0 || len(method) > maxABIMethodBytes {
		return fmt.Errorf("invalid method")
	}
	if length > maxABIRequestBytes {
		return fmt.Errorf("request too large")
	}
	return nil
}

//export cliproxyPluginFree
func cliproxyPluginFree(pointer unsafe.Pointer, _ C.size_t) {
	if pointer != nil {
		C.free(pointer)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { shutdown() }

func writeEnvelope(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	pointer := C.CBytes(raw)
	if pointer == nil {
		return
	}
	response.ptr = pointer
	response.len = C.size_t(len(raw))
}
