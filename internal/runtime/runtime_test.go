package runtime

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"

	"cpa-plugin-prompt-admin/internal/config"
	"cpa-plugin-prompt-admin/internal/engine"
	"cpa-plugin-prompt-admin/internal/inject"
)

func testEngine(t *testing.T) *engine.Engine {
	t.Helper()
	dir := t.TempDir()
	eng, err := engine.Open(config.Config{
		Enabled:       true,
		DataDir:       dir,
		CPAURL:        "http://127.0.0.1:9",
		Models:        []string{"demo-model"},
		WatchInterval: timeSecond,
		SyncInterval:  0,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	if err := eng.SetLocal("demo-model", "gateway prompt"); err != nil {
		t.Fatal(err)
	}
	return eng
}

const timeSecond = 1_000_000_000

func TestInjectsAndLabelsResponse(t *testing.T) {
	svc := New()
	svc.Use(testEngine(t), true)
	body := []byte(`{"model":"demo-model","messages":[{"role":"system","content":"client"},{"role":"user","content":"hi"}]}`)
	got := svc.InterceptBefore(pluginapi.RequestInterceptRequest{
		RequestID:    "req-1",
		SourceFormat: "openai",
		Model:        "demo-model",
		Body:         body,
		Metadata: map[string]any{
			"request_path": "/v1/chat/completions",
		},
	})
	msgs := gjson.GetBytes(got.Body, "messages").Array()
	if len(msgs) != 3 || msgs[0].Get("content").String() != "gateway prompt" || msgs[1].Get("content").String() != "client" {
		t.Fatalf("body = %s", got.Body)
	}
	resp := svc.InterceptResponse(pluginapi.ResponseInterceptRequest{RequestID: "req-1", RequestBody: got.Body})
	if resp.Headers.Get(inject.HeaderName) != inject.HeaderValue {
		t.Fatalf("headers = %v", resp.Headers)
	}

	again := svc.InterceptBefore(pluginapi.RequestInterceptRequest{
		RequestID:    "req-2",
		SourceFormat: "openai",
		Body:         got.Body,
		Metadata:     map[string]any{"request_path": "/v1/chat/completions"},
	})
	if len(again.Body) != 0 {
		t.Fatalf("retry changed the body: %s", again.Body)
	}
	stream := svc.InterceptStream(pluginapi.StreamChunkInterceptRequest{
		RequestID:   "req-2",
		RequestBody: got.Body,
		ChunkIndex:  pluginapi.StreamChunkHeaderInitIndex,
	})
	if stream.Headers.Get(inject.HeaderName) != inject.HeaderValue {
		t.Fatalf("stream headers = %v", stream.Headers)
	}
	payload := svc.InterceptStream(pluginapi.StreamChunkInterceptRequest{
		RequestID:  "req-2",
		ChunkIndex: 0,
		Body:       []byte("data: {}\n\n"),
	})
	if payload.Headers != nil || len(payload.Body) != 0 {
		t.Fatalf("payload chunk was rewritten: %+v", payload)
	}
}

func TestSkipNestedCallbackAndEmptyPrompt(t *testing.T) {
	svc := New()
	svc.Use(testEngine(t), true)
	body := []byte(`{"model":"other","messages":[{"role":"user","content":"hi"}]}`)
	got := svc.InterceptBefore(pluginapi.RequestInterceptRequest{
		RequestID: "nested",
		Body:      body,
		Metadata:  map[string]any{"source": inject.NestedSource, "request_path": "/v1/chat/completions"},
	})
	if len(got.Body) != 0 {
		t.Fatal("nested callback was rewritten")
	}
	plain := svc.InterceptBefore(pluginapi.RequestInterceptRequest{
		RequestID:    "no-prompt",
		SourceFormat: "openai",
		Body:         body,
		Metadata:     map[string]any{"request_path": "/v1/chat/completions"},
	})
	if len(plain.Body) != 0 {
		t.Fatal("model without a prompt was rewritten")
	}
	if svc.InterceptResponse(pluginapi.ResponseInterceptRequest{RequestID: "no-prompt"}).Headers != nil {
		t.Fatal("header set without injection")
	}
}

func TestDisabledSkipsInjection(t *testing.T) {
	svc := New()
	eng := testEngine(t)
	svc.Use(eng, false)
	got := svc.InterceptBefore(pluginapi.RequestInterceptRequest{
		SourceFormat: "openai",
		Body:         []byte(`{"model":"demo-model","messages":[{"role":"user","content":"hi"}]}`),
		Metadata:     map[string]any{"request_path": "/v1/chat/completions"},
	})
	if len(got.Body) != 0 {
		t.Fatal("disabled plugin injected")
	}
}

func TestReconfigureKeepsPreviousEngine(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	raw := []byte("data_dir: " + filepath.ToSlash(dir) + "\nmodels: [demo-model]\nsync_interval: 0\nwatch_interval: 1h\ncpa_url: http://127.0.0.1:9\n")
	if err := svc.Configure(raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Shutdown)
	if err := svc.Configure([]byte("not_a_field: true\n")); err == nil {
		t.Fatal("unknown config was accepted")
	}
	if !svc.Ready() {
		t.Fatal("failed reconfigure dropped the engine")
	}
	page := svc.HandleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/prompt-admin/index.html"})
	if page.StatusCode != http.StatusOK || !strings.Contains(string(page.Body), "立即同步") || !strings.Contains(string(page.Body), "保留这个") {
		t.Fatalf("page status %d", page.StatusCode)
	}
	state := svc.HandleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/plugins/prompt-admin/state"})
	if state.StatusCode != http.StatusOK || !strings.Contains(string(state.Body), `"repos"`) {
		t.Fatalf("state = %d %s", state.StatusCode, state.Body)
	}
}

func TestRegistrationShape(t *testing.T) {
	reg := ManagementRegistration()
	if len(reg.Resources) != 1 || reg.Resources[0].Path != "/index.html" || reg.Resources[0].Menu != "模型提示词" {
		t.Fatalf("resource = %+v", reg.Resources)
	}
	seen := map[string]bool{}
	for _, route := range reg.Routes {
		seen[route.Method+" "+route.Path] = true
	}
	for _, path := range []string{"/state", "/file", "/sync", "/repos/add", "/choose", "/default"} {
		method := http.MethodPost
		if path == "/state" || path == "/file" {
			method = http.MethodGet
		}
		if !seen[method+" /plugins/prompt-admin"+path] {
			t.Fatalf("missing %s %s in %+v", method, path, reg.Routes)
		}
	}
	if Metadata().Version == "" || Metadata().GitHubRepository == "" {
		t.Fatalf("metadata = %+v", Metadata())
	}
}
