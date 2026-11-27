package engine

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"cpa-plugin-prompt-admin/internal/config"
	"gopkg.in/yaml.v3"
)

type modelSnap struct {
	IDs      []string
	Upstream map[string]string
	Source   string
	Error    string
}

type namesDoc struct {
	Aliases  map[string]string
	Provider map[string]string
	Pointer  map[string]bool
}

type signature struct {
	Hash        string `json:"hash"`
	Source      string `json:"source"`
	Error       string `json:"error,omitempty"`
	ConfigMtime int64  `json:"config_mtime"`
	NamesMtime  int64  `json:"names_mtime"`
}

func (e *Engine) fetchModels() modelSnap {
	if e.fetch != nil {
		return e.fetch()
	}
	host, configErr := readCPAConfig(e.cfg.CPAConfig)
	key := host.Key
	if strings.TrimSpace(e.cfg.APIKey) != "" {
		key = strings.TrimSpace(e.cfg.APIKey)
	}
	base := e.cfg.CPAURL
	if base == "" || (!e.cfg.CPAURLExplicit && base == config.DefaultCPAURL && host.URL != "") {
		base = host.URL
	}
	if base == "" {
		base = config.DefaultCPAURL
	}
	ids, err := e.fetchModelIDs(base, key)
	if err == nil {
		return modelSnap{IDs: ids, Upstream: host.Upstream, Source: "CPA /v1/models"}
	}
	message := err.Error()
	if configErr != nil && e.cfg.APIKey == "" {
		message += "; " + configErr.Error()
	}
	if key == "" {
		message += "; 未找到客户端 API Key：请检查 cpa_config 的 access.api-keys（旧版 api-keys），或设置插件 api_key；不要使用 Management Key"
	}
	snap := modelSnap{Upstream: host.Upstream, Source: "none", Error: message}
	if len(host.Upstream) > 0 {
		for alias := range host.Upstream {
			snap.IDs = append(snap.IDs, alias)
		}
		sort.Strings(snap.IDs)
		snap.Source = "config.yaml (fallback)"
	} else if len(e.cfg.Models) > 0 {
		snap.IDs = uniqueModels(e.cfg.Models)
		snap.Source = "plugin config"
	}
	return snap
}

func (e *Engine) fetchModelIDs(base, key string) ([]string, error) {
	endpoint, err := modelsEndpoint(base)
	if err != nil {
		return nil, UserError("cpa_url 必须是有效的 HTTP(S) URL")
	}
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, UserError("无法创建模型列表请求")
	}
	// /v1/models authenticates CLIENT API keys, never a management secret.
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, Transport: e.httpClient().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %s (检查容器内地址、端口、TLS 和 cpa_url)", endpoint, redactModelError(err.Error(), key))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: 读取响应失败", endpoint)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := responseMessage(body)
		return nil, fmt.Errorf("GET %s: HTTP %d %s%s", endpoint, resp.StatusCode, http.StatusText(resp.StatusCode), redactModelError(detail, key))
	}
	if len(body) > 4<<20 {
		return nil, fmt.Errorf("GET %s: HTTP %d 响应超过 4 MiB", endpoint, resp.StatusCode)
	}
	ids, err := parseModelIDs(body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: HTTP %d %s", endpoint, resp.StatusCode, redactModelError(err.Error(), key))
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("GET %s: HTTP %d 模型列表为空；请先在 CPA 配置并启用可用的上游凭据", endpoint, resp.StatusCode)
	}
	return ids, nil
}

func modelsEndpoint(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/v1/models") {
		return u.String(), nil
	}
	if strings.HasSuffix(u.Path, "/v1") {
		u.Path += "/models"
	} else {
		u.Path += "/v1/models"
	}
	return u.String(), nil
}

func responseMessage(body []byte) string {
	var doc struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return "；响应不是 JSON（检查反向代理/Cloudflare Access 登录或重定向）"
	}
	var message string
	_ = json.Unmarshal(doc.Error, &message)
	if message == "" {
		var nested struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(doc.Error, &nested)
		message = nested.Message
	}
	if message == "" {
		message = doc.Message
	}
	if message == "" {
		return ""
	}
	return ": " + trimRunes(message, 300)
}

func redactModelError(message, key string) string {
	if key != "" {
		message = strings.ReplaceAll(message, key, "[redacted]")
	}
	return message
}

func parseModelIDs(body []byte) ([]string, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, UserError("响应不是有效 JSON（检查代理是否返回 HTML 登录页）")
	}
	if doc, ok := value.(map[string]any); ok {
		if _, hasError := doc["error"]; hasError {
			return nil, UserError("模型接口返回错误" + responseMessage(body))
		}
		if data, exists := doc["data"]; exists {
			value = data
		} else if models, exists := doc["models"]; exists {
			value = models
		} else {
			return nil, UserError("无法识别模型 JSON：缺少 data/models 数组")
		}
	}
	list, ok := value.([]any)
	if !ok {
		return nil, UserError("无法识别模型 JSON：模型列表必须是数组")
	}
	var ids []string
	for _, item := range list {
		id, ok := item.(string)
		if !ok {
			obj, valid := item.(map[string]any)
			if !valid {
				return nil, UserError("模型条目必须是字符串或含 id/name 的对象")
			}
			id, _ = obj["id"].(string)
			if strings.TrimSpace(id) == "" {
				id, _ = obj["name"].(string)
			}
		}
		if strings.TrimSpace(id) == "" {
			return nil, UserError("模型条目缺少有效的 id/name")
		}
		ids = append(ids, id)
	}
	return uniqueModels(ids), nil
}

func uniqueModels(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

type cpaConfig struct {
	Key, URL string
	Upstream map[string]string
}

func readCPAConfig(path string) (cpaConfig, error) {
	result := cpaConfig{Upstream: map[string]string{}}
	if strings.TrimSpace(path) == "" {
		return result, UserError("未指定 cpa_config")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return result, fmt.Errorf("无法读取 cpa_config %q（使用容器内路径）", path)
	}
	var doc map[string]any
	if yaml.Unmarshal(raw, &doc) != nil {
		return result, UserError("cpa_config 不是有效的 YAML 配置")
	}
	// New access.api-keys takes precedence even when explicitly empty.
	keys := doc["api-keys"]
	if access, ok := doc["access"].(map[string]any); ok {
		if v, exists := access["api-keys"]; exists {
			keys = v
		}
	}
	if list, ok := keys.([]any); ok {
		for _, item := range list {
			if key, ok := item.(string); ok && strings.TrimSpace(key) != "" {
				result.Key = strings.TrimSpace(key)
				break
			}
		}
	}
	server := doc
	if v, ok := doc["server"].(map[string]any); ok {
		server = v
	}
	host, _ := server["host"].(string)
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	port := 8317
	if v, ok := server["port"].(int); ok && v > 0 && v < 65536 {
		port = v
	}
	scheme := "http"
	if tls, ok := server["tls"].(map[string]any); ok && tls["enable"] == true {
		scheme = "https"
	}
	result.URL = scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
	// Restrict traversal to provider/model trees; never scan plugin config,
	// management.secret-key or api-key fields for client credentials.
	for _, field := range []string{"api-keys", "oauth", "oauth-model-alias", "gemini-api-key", "claude-api-key", "codex-api-key", "vertex-api-key", "openai-compatibility"} {
		collectAliases(doc[field], result.Upstream)
	}
	return result, nil
}

func collectAliases(value any, aliases map[string]string) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			collectAliases(item, aliases)
		}
	case map[string]any:
		name, _ := value["name"].(string)
		alias, _ := value["alias"].(string)
		if name != "" && alias != "" {
			aliases[strings.TrimSpace(alias)] = strings.TrimSpace(name)
		}
		for _, child := range value {
			collectAliases(child, aliases)
		}
	}
}

func loadNames(path string) namesDoc {
	doc := namesDoc{Aliases: map[string]string{}, Provider: map[string]string{}, Pointer: map[string]bool{}}
	if strings.TrimSpace(path) == "" {
		return doc
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc
	}
	var payload struct {
		Aliases map[string]string `json:"aliases"`
		Models  []struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"models"`
		Pointers []struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"pointers"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return doc
	}
	for old, next := range payload.Aliases {
		if strings.TrimSpace(next) == "" {
			continue
		}
		doc.Aliases[old] = next
	}
	for _, model := range payload.Models {
		if model.Name != "" {
			doc.Provider[model.Name] = model.Provider
		}
	}
	for _, pointer := range payload.Pointers {
		if pointer.Name != "" {
			doc.Pointer[pointer.Name] = true
			if _, ok := doc.Provider[pointer.Name]; !ok {
				doc.Provider[pointer.Name] = pointer.Provider
			}
		}
	}
	return doc
}

func (e *Engine) signature(snap modelSnap) signature {
	sum := sha1.Sum([]byte(strings.Join(snap.IDs, "\n")))
	return signature{
		Hash:        hex.EncodeToString(sum[:])[:16],
		Source:      snap.Source,
		Error:       snap.Error,
		ConfigMtime: fileMtime(e.cfg.CPAConfig),
		NamesMtime:  fileMtime(e.cfg.NamesFile),
	}
}

func fileMtime(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}
