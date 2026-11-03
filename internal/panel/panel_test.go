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
	body := string(page.Body)
	if page.Status != http.StatusOK || !strings.Contains(body, "模型提示词管理") {
		t.Fatalf("status %d", page.Status)
	}
	if strings.Contains(body, `placeholder="CPA 管理密钥"`) || strings.Contains(body, `id="pw"`) || strings.Contains(body, "loginBtn") || strings.Contains(body, "忘记本页密钥") {
		t.Fatal("management page must not offer a key paste / login form")
	}
	if !strings.Contains(body, "从管理中心菜单重新打开") || !strings.Contains(body, "记住密码") {
		t.Fatal("expected Management Center notice with the 记住密码 hint")
	}
	for _, want := range []string{"enc::v2::", "enc::v1::", "cli-proxy-api-webui::secure-storage", "navigator.userAgent", "cli-proxy-auth"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in the Management Center storage decoder", want)
		}
	}
	if !strings.Contains(body, "same-origin") {
		t.Fatal("expected same-origin credentials on management fetches")
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
