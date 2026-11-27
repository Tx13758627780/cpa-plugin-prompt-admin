package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cpa-plugin-prompt-admin/internal/config"
)

func TestReadCPAConfigLayouts(t *testing.T) {
	for _, tc := range []struct{ name, yaml, key, address string }{
		{"v8 inline", `server: {port: 9417}
management: {secret-key: management-secret}
access: {api-keys: [client-secret]}
api-keys:
  codex:
    - models: [{name: upstream, alias: public}]
`, "client-secret", "http://127.0.0.1:9417"},
		{"v8 indentless", "access:\n  api-keys:\n  - 'client # secret'\noauth:\n  model-alias:\n    codex: [{name: upstream, alias: public}]\n", "client # secret", "http://127.0.0.1:8317"},
		{"legacy", `port: 9418
remote-management: {secret-key: management-secret}
api-keys: [legacy-client]
openai-compatibility:
  - name: provider
    models: [{name: upstream, alias: public}]
`, "legacy-client", "http://127.0.0.1:9418"},
		{"v8 precedence and tls", `server: {host: "::", port: 9443, tls: {enable: true}}
port: 1234
api-keys: [legacy-client]
access: {api-keys: []}
oauth-model-alias: {codex: [{name: upstream, alias: public}]}
`, "", "https://[::1]:9443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, path, tc.yaml)
			got, err := readCPAConfig(path)
			if err != nil || got.Key != tc.key || got.URL != tc.address || got.Upstream["public"] != "upstream" {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "invalid.yaml")
	writeFile(t, path, "access: [secret-content")
	if _, err := readCPAConfig(path); err == nil || strings.Contains(err.Error(), "secret-content") {
		t.Fatalf("invalid YAML error: %v", err)
	}
	if _, err := readCPAConfig(path + "missing"); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestModelHTTPResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       []string
		errorText  string
	}{
		{"openai", `{"data":[{"id":" b "},{"id":"a"},{"id":"a"}]}`, 200, []string{"a", "b"}, ""},
		{"older models", `{"models":[{"name":"b"},{"id":"a"}]}`, 200, []string{"a", "b"}, ""},
		{"array", `["b","a"]`, 200, []string{"a", "b"}, ""},
		{"empty", `{"data":[]}`, 200, nil, "模型列表为空"},
		{"unknown", `{"items":[]}`, 200, nil, "缺少 data/models"},
		{"invalid entry", `{"data":[{"id":"a"},{}]}`, 200, nil, "缺少有效的 id/name"},
		{"null", `{"data":null}`, 200, nil, "必须是数组"},
		{"html", `<html>Login</html>`, 200, nil, "不是有效 JSON"},
		{"unauthorized", `{"error":{"message":"Invalid API key"}}`, 401, nil, "HTTP 401 Unauthorized: Invalid API key"},
		{"forbidden", `{"error":"blocked"}`, 403, nil, "HTTP 403 Forbidden: blocked"},
		{"missing", `404 page not found`, 404, nil, "HTTP 404"},
		{"redirect", `<html>Access login</html>`, 302, nil, "HTTP 302"},
		{"error in success", `{"error":"bad key client-secret","data":[]}`, 200, nil, "bad key [redacted]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/prefix/v1/models" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer client-secret" || r.Header.Get("X-Management-Key") != "" {
					t.Errorf("request %s %s auth=%q", r.Method, r.URL, r.Header.Get("Authorization"))
				}
				if tc.status == 302 {
					w.Header().Set("Location", "/unexpected")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			eng, err := Open(config.Config{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			ids, err := eng.fetchModelIDs(server.URL+"/prefix/v1/", "client-secret")
			if tc.errorText == "" {
				if err != nil || !reflect.DeepEqual(ids, tc.want) {
					t.Fatalf("%v %v", ids, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.errorText) || strings.Contains(err.Error(), "client-secret") {
				t.Fatalf("error=%v", err)
			}
			if calls != 1 {
				t.Fatalf("unexpected redirect/retry: %d calls", calls)
			}
		})
	}
}

func TestModelsEndpointURLs(t *testing.T) {
	for _, path := range []string{"", "/v1", "/v1/", "/v1/models"} {
		got, err := modelsEndpoint("https://proxy.example.com/prefix" + path)
		if err != nil || got != "https://proxy.example.com/prefix/v1/models" {
			t.Fatalf("%s: %s %v", path, got, err)
		}
	}
	for _, base := range []string{"ftp://proxy.example.com", "https://secret@example.com", "https://proxy.example.com?key=secret"} {
		if _, err := modelsEndpoint(base); err == nil {
			t.Fatalf("accepted %s", base)
		}
	}
}

func TestFreshV8ConfigDiscoversModelsWithoutPluginAPIKey(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer client-secret" {
			t.Errorf("wrong auth: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"live-model"}]}`)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, fmt.Sprintf("server: {port: %s}\nmanagement: {secret-key: management-secret}\naccess: {api-keys: [client-secret]}\n", u.Port()))
	cfg, err := config.Parse([]byte("data_dir: " + t.TempDir() + "\ncpa_config: " + path + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	snap := eng.Snapshot()
	if snap.Status.ModelError != "" || !reflect.DeepEqual(snap.Status.Models, []string{"live-model"}) {
		t.Fatalf("%+v", snap.Status)
	}
	if calls != 1 {
		t.Fatalf("state should fetch once, got %d", calls)
	}
}

func TestAPIKeyOverrideAndUnauthenticatedDiscovery(t *testing.T) {
	for _, key := range []string{"", "explicit-client"} {
		t.Run(key, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := ""
				if key != "" {
					want = "Bearer " + key
				}
				if r.Header.Get("Authorization") != want {
					t.Errorf("auth %q != %q", r.Header.Get("Authorization"), want)
				}
				fmt.Fprint(w, `{"data":[{"id":"a"}]}`)
			}))
			defer server.Close()
			cfg := config.Config{DataDir: t.TempDir(), CPAURL: server.URL, APIKey: key}
			eng, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			if snap := eng.fetchModels(); snap.Error != "" || len(snap.IDs) != 1 {
				t.Fatalf("%+v", snap)
			}
		})
	}
}

func TestModelFailureManualFallbackAndRecovery(t *testing.T) {
	eng, err := Open(config.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	failed := false
	eng.fetch = func() modelSnap {
		if failed {
			return modelSnap{Source: "none", Error: "HTTP 401 Unauthorized: Invalid API key"}
		}
		return modelSnap{IDs: []string{"live"}, Source: "test"}
	}
	if err := eng.SetLocal("live", "keep prompt"); err != nil {
		t.Fatal(err)
	}
	failed = true
	snap := eng.Snapshot()
	if !strings.Contains(snap.Status.ModelError, "HTTP 401") || !strings.Contains(snap.Status.Error, "HTTP 401") {
		t.Fatalf("diagnostics %+v", snap.Status)
	}
	if err := eng.SetManualModel("manual", false); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetLocal("manual", "manual prompt"); err != nil {
		t.Fatal(err)
	}
	prompts, _ := eng.Prompts()
	if prompts["manual"] != "manual prompt" || prompts["live"] != "keep prompt" {
		t.Fatalf("outage erased prompts: %v", prompts)
	}
	if disk := readPromptFile(eng.promptsPath()).Prompts; disk["live"] != "keep prompt" || disk["manual"] != "manual prompt" {
		t.Fatalf("outage erased prompts on disk: %v", disk)
	}
	if !reflect.DeepEqual(loadState(eng.statePath()).ManualModels, []string{"manual"}) {
		t.Fatal("manual model not persisted")
	}
	if err := eng.SetManualModel("manual", false); err != nil {
		t.Fatal(err)
	}
	if len(eng.CachedSnapshot().ManualModels) != 1 {
		t.Fatal("duplicate manual model")
	}
	failed = false
	snap = eng.Snapshot()
	if snap.Status.ModelError != "" || snap.Status.Error != "" || len(snap.Status.Models) != 2 {
		t.Fatalf("recovery %+v", snap.Status)
	}
	if err := eng.SetManualModel("manual", true); err != nil {
		t.Fatal(err)
	}
	prompts, _ = eng.Prompts()
	if _, ok := prompts["manual"]; ok {
		t.Fatalf("removed manual prompt still injected: %v", prompts)
	}
	if err := eng.SetManualModel("\n", false); err == nil {
		t.Fatal("blank model accepted")
	}
}

func TestFallbackKeepsHTTPDiagnostic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); fmt.Fprint(w, `{"error":"denied"}`) }))
	defer server.Close()
	eng, err := Open(config.Config{DataDir: t.TempDir(), CPAURL: server.URL, APIKey: "client-secret", Models: []string{"fallback"}})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	snap := eng.Snapshot()
	body, _ := json.Marshal(snap)
	if len(snap.Status.Models) != 1 || !strings.Contains(snap.Status.ModelError, "HTTP 403 Forbidden: denied") || strings.Contains(string(body), "client-secret") {
		t.Fatalf("%s", body)
	}
	if data, err := os.ReadFile(eng.statusPath()); err != nil || !strings.Contains(string(data), "HTTP 403") {
		t.Fatalf("diagnostic not saved: %v", err)
	}
}
