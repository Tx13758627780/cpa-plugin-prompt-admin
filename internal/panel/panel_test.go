package panel

import (
	"net/http"
	"strings"
	"testing"

	"cpa-plugin-prompt-admin/internal/config"
	"cpa-plugin-prompt-admin/internal/engine"
)

func TestPageAndDefaultPrompt(t *testing.T) {
	eng, err := engine.Open(config.Config{
		Enabled:       true,
		DataDir:       t.TempDir(),
		CPAURL:        "http://127.0.0.1:9",
		Models:        []string{"demo-model"},
		WatchInterval: 1_000_000_000,
		SyncInterval:  0,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	page := Handle(eng, http.MethodGet, "/v0/resource/plugins/prompt-admin/index.html", nil, nil)
	if page.Status != http.StatusOK || !strings.Contains(string(page.Body), "模型提示词管理") {
		t.Fatalf("status %d", page.Status)
	}
	saved := Handle(eng, http.MethodPost, "/plugins/prompt-admin/default", nil, []byte(`{"text":"fallback"}`))
	if saved.Status != http.StatusOK {
		t.Fatalf("save = %d %s", saved.Status, saved.Body)
	}
	state := Handle(eng, http.MethodGet, "/state", nil, nil)
	if !strings.Contains(string(state.Body), "fallback") {
		t.Fatalf("state = %s", state.Body)
	}
}
