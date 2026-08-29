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
static cliproxy_host_api retained_host;
static int has_retained_host;
typedef struct { cliproxy_host_api api; } cliproxy_host_snapshot;
static void retain_host_api(const cliproxy_host_api* host) {
	if (host == NULL) { return; }
	retained_host = *host;
	has_retained_host = 1;
}
static void clear_retained_host(void) {
	memset(&retained_host, 0, sizeof(retained_host));
	has_retained_host = 0;
}
static cliproxy_host_snapshot* snapshot_retained_host(void) {
	if (!has_retained_host || retained_host.call == NULL) { return NULL; }
	cliproxy_host_snapshot* snapshot = malloc(sizeof(cliproxy_host_snapshot));
	if (snapshot == NULL) { return NULL; }
	snapshot->api = retained_host;
	return snapshot;
}
static void free_host_snapshot(cliproxy_host_snapshot* snapshot) {
	if (snapshot != NULL) { free(snapshot); }
}
static int call_host_snapshot(const cliproxy_host_snapshot* snapshot, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (response != NULL) { response->ptr = NULL; response->len = 0; }
	if (snapshot == NULL || snapshot->api.call == NULL || method == NULL) { return 1; }
	return snapshot->api.call(snapshot->api.host_ctx, method, request, request_len, response);
}
static void free_host_snapshot_buffer(const cliproxy_host_snapshot* snapshot, void* pointer, size_t length) {
	if (pointer != NULL && snapshot != NULL && snapshot->api.free_buffer != NULL) {
		snapshot->api.free_buffer(pointer, length);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
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
	nativeHost.mu.Lock()
	C.retain_host_api(host)
	nativeHost.mu.Unlock()
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
func cliproxyPluginShutdown() {
	nativeHost.lifecycle.stop()
	shutdown()
	nativeHost.lifecycle.wait()
	nativeHost.mu.Lock()
	C.clear_retained_host()
	nativeHost.mu.Unlock()
}

type nativeStreamLifecycle struct {
	mu         sync.Mutex
	stopping   bool
	forwarders sync.WaitGroup
}

func (lifecycle *nativeStreamLifecycle) begin() (func(), bool) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.stopping {
		return nil, false
	}
	lifecycle.forwarders.Add(1)
	return lifecycle.forwarders.Done, true
}

func (lifecycle *nativeStreamLifecycle) stop() {
	lifecycle.mu.Lock()
	lifecycle.stopping = true
	lifecycle.mu.Unlock()
}

func (lifecycle *nativeStreamLifecycle) wait() {
	lifecycle.forwarders.Wait()
}

var nativeHost struct {
	mu        sync.Mutex
	lifecycle nativeStreamLifecycle
}

type nativeStreamLease struct {
	snapshot *C.cliproxy_host_snapshot
	release  func()
	once     sync.Once
}

func acquireNativeStreamLease() (*nativeStreamLease, error) {
	release, ok := nativeHost.lifecycle.begin()
	if !ok {
		return nil, fmt.Errorf("plugin is shutting down")
	}
	nativeHost.mu.Lock()
	snapshot := C.snapshot_retained_host()
	nativeHost.mu.Unlock()
	if snapshot == nil {
		release()
		return nil, fmt.Errorf("native host callback is unavailable")
	}
	return &nativeStreamLease{snapshot: snapshot, release: release}, nil
}

func (lease *nativeStreamLease) finish(err error) {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		if err != nil {
			log.Printf("cliproxy-cursor-acp: native stream bridge failed: %v", err)
		}
		C.free_host_snapshot(lease.snapshot)
		lease.snapshot = nil
		if lease.release != nil {
			lease.release()
		}
	})
}

func (lease *nativeStreamLease) callHost(method string, request []byte) error {
	if lease == nil || lease.snapshot == nil {
		return fmt.Errorf("native host callback is unavailable")
	}
	methodName := C.CString(method)
	defer C.free(unsafe.Pointer(methodName))
	var requestPointer *C.uint8_t
	if len(request) > 0 {
		requestPointer = (*C.uint8_t)(unsafe.Pointer(&request[0]))
	}
	var response C.cliproxy_buffer
	result := C.call_host_snapshot(lease.snapshot, methodName, requestPointer, C.size_t(len(request)), &response)
	var raw []byte
	if response.ptr != nil && response.len > 0 {
		if uint64(response.len) > maxABIRequestBytes {
			C.free_host_snapshot_buffer(lease.snapshot, response.ptr, response.len)
			return fmt.Errorf("host callback response is too large")
		}
		raw = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_snapshot_buffer(lease.snapshot, response.ptr, response.len)
	}
	if result != 0 {
		return fmt.Errorf("host callback %q failed", method)
	}
	if len(raw) == 0 {
		return nil
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		return fmt.Errorf("host callback %q returned an error", method)
	}
	return nil
}

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
