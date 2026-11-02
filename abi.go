package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int PromptAdminPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void PromptAdminPluginFree(void*, size_t);
extern void PromptAdminPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"cpa-plugin-prompt-admin/internal/runtime"
)

func main() {}

// registrationSchemaVersion stays at 1 so hosts that have not adopted schema 2 still load the plugin.
// Response and management capabilities are selected by flags, not by the schema number.
const registrationSchemaVersion uint32 = 1

const maxCGoRequestLen = C.size_t(1<<31 - 1)

type abiEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *abiError       `json:"error,omitempty"`
}

type abiError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type abiRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  abiCapabilities    `json:"capabilities"`
}

type abiCapabilities struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
	ManagementAPI          bool `json:"management_api"`
}

type requestInterceptRPC struct {
	pluginapi.RequestInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type responseInterceptRPC struct {
	pluginapi.ResponseInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type streamInterceptRPC struct {
	pluginapi.StreamChunkInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRPCRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

var abiState = struct {
	sync.Mutex
	service      *runtime.Service
	shuttingDown bool
	inFlight     sync.WaitGroup
}{
	service: runtime.New(),
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	abiState.Lock()
	abiState.shuttingDown = false
	abiState.Unlock()
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.PromptAdminPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.PromptAdminPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.PromptAdminPluginShutdown)
	return 0
}

//export PromptAdminPluginCall
func PromptAdminPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeABIResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 0
	}
	if requestLen > maxCGoRequestLen {
		writeABIResponse(response, errorEnvelope("request_too_large", "request payload is too large", 0))
		return 0
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		raw = errorEnvelope("plugin_error", err.Error(), 0)
	}
	writeABIResponse(response, raw)
	return 0
}

//export PromptAdminPluginFree
func PromptAdminPluginFree(pointer unsafe.Pointer, _ C.size_t) {
	if pointer != nil {
		C.free(pointer)
	}
}

//export PromptAdminPluginShutdown
func PromptAdminPluginShutdown() {
	abiState.Lock()
	abiState.shuttingDown = true
	service := abiState.service
	abiState.Unlock()
	abiState.inFlight.Wait()
	if service != nil {
		service.Shutdown()
	}
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return registerPlugin(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(runtime.ManagementRegistration())
	case pluginabi.MethodManagementHandle:
		var rpc managementRPCRequest
		if err := json.Unmarshal(request, &rpc); err != nil {
			return nil, fmt.Errorf("decode management.handle request: %w", err)
		}
		service, done, err := beginCall()
		if err != nil {
			return nil, err
		}
		defer done()
		return okEnvelope(service.HandleManagement(rpc.ManagementRequest))
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		var rpc requestInterceptRPC
		if err := json.Unmarshal(request, &rpc); err != nil {
			return nil, fmt.Errorf("decode %s request: %w", method, err)
		}
		service, done, err := beginCall()
		if err != nil {
			return nil, err
		}
		defer done()
		if method == pluginabi.MethodRequestInterceptAfter {
			return okEnvelope(service.InterceptAfter(rpc.RequestInterceptRequest))
		}
		return okEnvelope(service.InterceptBefore(rpc.RequestInterceptRequest))
	case pluginabi.MethodResponseInterceptAfter:
		var rpc responseInterceptRPC
		if err := json.Unmarshal(request, &rpc); err != nil {
			return nil, fmt.Errorf("decode response.intercept_after request: %w", err)
		}
		service, done, err := beginCall()
		if err != nil {
			return nil, err
		}
		defer done()
		return okEnvelope(service.InterceptResponse(rpc.ResponseInterceptRequest))
	case pluginabi.MethodResponseInterceptStreamChunk:
		var rpc streamInterceptRPC
		if err := json.Unmarshal(request, &rpc); err != nil {
			return nil, fmt.Errorf("decode response.intercept_stream_chunk request: %w", err)
		}
		service, done, err := beginCall()
		if err != nil {
			return nil, err
		}
		defer done()
		return okEnvelope(service.InterceptStream(rpc.StreamChunkInterceptRequest))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

func registerPlugin(raw []byte) ([]byte, error) {
	var request lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	service, done, err := beginConfigure()
	if err != nil {
		return nil, err
	}
	defer done()
	if err := service.Configure(request.ConfigYAML); err != nil {
		return nil, err
	}
	return okEnvelope(abiRegistration{
		SchemaVersion: registrationSchemaVersion,
		Metadata:      runtime.Metadata(),
		Capabilities: abiCapabilities{
			RequestInterceptor:     true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
			ManagementAPI:          true,
		},
	})
}

func beginCall() (*runtime.Service, func(), error) {
	abiState.Lock()
	defer abiState.Unlock()
	if abiState.shuttingDown {
		return nil, nil, fmt.Errorf("prompt-admin is shutting down")
	}
	if abiState.service == nil || !abiState.service.Ready() {
		return nil, nil, fmt.Errorf("prompt-admin is not registered")
	}
	abiState.inFlight.Add(1)
	return abiState.service, abiState.inFlight.Done, nil
}

// beginConfigure allows the first register call before the engine exists.
func beginConfigure() (*runtime.Service, func(), error) {
	abiState.Lock()
	defer abiState.Unlock()
	if abiState.shuttingDown {
		return nil, nil, fmt.Errorf("prompt-admin is shutting down")
	}
	if abiState.service == nil {
		abiState.service = runtime.New()
	}
	abiState.inFlight.Add(1)
	return abiState.service, abiState.inFlight.Done, nil
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(abiEnvelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(abiEnvelope{OK: false, Error: &abiError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

func writeABIResponse(response *C.cliproxy_buffer, raw []byte) {
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
