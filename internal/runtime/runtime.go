// Package runtime is the in-process CPA adapter: config lifecycle, request
// injection, response headers, and the management page.
package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"

	"cpa-plugin-prompt-admin/internal/config"
	"cpa-plugin-prompt-admin/internal/engine"
	"cpa-plugin-prompt-admin/internal/inject"
	"cpa-plugin-prompt-admin/internal/panel"
)

const (
	pluginID       = panel.PluginID
	pluginName     = "Prompt Admin"
	pluginAuthor   = "CPA Prompt Admin contributors"
	pluginRepo     = "https://github.com/Tx13758627780/cpa-plugin-prompt-admin"
	markTTL        = 10 * time.Minute
	maxMarkEntries = 8192
)

// Version is set by the main package from ldflags.
var Version = "0.1.2"

// Service owns one engine and the short-lived injection marks used to label responses.
type Service struct {
	mu      sync.Mutex
	eng     *engine.Engine
	enabled bool
	marks   *markStore
}

// New returns a service that rejects calls until Configure succeeds.
func New() *Service {
	return &Service{marks: newMarkStore()}
}

// Configure validates YAML, swaps in a new engine, and starts its watcher.
// A validation or open error leaves the previous engine running.
func (s *Service) Configure(raw []byte) error {
	if s == nil {
		return engine.UserError("plugin is not ready")
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return err
	}
	eng, err := engine.Open(cfg)
	if err != nil {
		return err
	}
	eng.Start()
	s.mu.Lock()
	old := s.eng
	s.eng = eng
	s.enabled = cfg.Enabled
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

// Use installs an already opened engine. Tests use it to avoid the watcher.
func (s *Service) Use(eng *engine.Engine, enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	old := s.eng
	s.eng = eng
	s.enabled = enabled && eng != nil && eng.Active()
	s.mu.Unlock()
	if old != nil && old != eng {
		old.Close()
	}
}

// Shutdown stops the watcher and drops the engine.
func (s *Service) Shutdown() {
	if s == nil {
		return
	}
	s.mu.Lock()
	eng := s.eng
	s.eng = nil
	s.enabled = false
	s.mu.Unlock()
	if eng != nil {
		eng.Close()
	}
}

// Ready reports whether an engine is installed.
func (s *Service) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eng != nil
}

func (s *Service) current() (*engine.Engine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eng, s.enabled && s.eng != nil && s.eng.Active()
}

// InterceptBefore prepends the resolved system prompt on the inbound payload.
// Nested host.model callbacks are left untouched. An unchanged body is returned
// empty so CPA keeps the payload it already has.
func (s *Service) InterceptBefore(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	eng, ok := s.current()
	if !ok || metadataString(req.Metadata, "source") == inject.NestedSource {
		return pluginapi.RequestInterceptResponse{}
	}
	prompts, aliases := eng.Prompts()
	model := requestModel(req)
	result := inject.Apply(inject.Request{
		SourceFormat: req.SourceFormat,
		Model:        model,
		Body:         req.Body,
		Prompt:       promptFor(prompts, aliases, req.Body, model),
		Aliases:      aliases,
		RequestPath:  metadataString(req.Metadata, "request_path"),
	})
	if result.Injected || result.AliasedFrom != "" {
		final := req.Body
		if result.Changed {
			final = result.Body
		}
		s.marks.put(req.RequestID, final, result.Injected, aliasHeader(result))
	}
	if !result.Changed || len(result.Body) == 0 {
		return pluginapi.RequestInterceptResponse{}
	}
	return pluginapi.RequestInterceptResponse{Body: result.Body}
}

// InterceptAfter returns no changes. Injection happens once, before auth selection.
func (s *Service) InterceptAfter(pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

// InterceptResponse attaches X-Prompt-Admin and X-Model-Alias when this request was rewritten.
func (s *Service) InterceptResponse(req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	if s == nil {
		return pluginapi.ResponseInterceptResponse{}
	}
	stamp, ok := s.marks.get(req.RequestID, req.RequestBody)
	if !ok {
		return pluginapi.ResponseInterceptResponse{}
	}
	return pluginapi.ResponseInterceptResponse{Headers: stamp.headers()}
}

// InterceptStream sets the same headers on the header-only stream init chunk.
func (s *Service) InterceptStream(req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	if s == nil || req.ChunkIndex != pluginapi.StreamChunkHeaderInitIndex {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	stamp, ok := s.marks.get(req.RequestID, req.RequestBody)
	if !ok {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	return pluginapi.StreamChunkInterceptResponse{Headers: stamp.headers()}
}

// HandleManagement serves the embedded page and its JSON actions.
func (s *Service) HandleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	eng, _ := s.currentEngine()
	path, query := splitQuery(req.Path, req.Query)
	result := panel.Handle(eng, req.Method, path, query, req.Body)
	status := result.Status
	if status == 0 {
		status = http.StatusOK
	}
	return pluginapi.ManagementResponse{StatusCode: status, Headers: result.Header, Body: result.Body}
}

func (s *Service) currentEngine() (*engine.Engine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eng, s.eng != nil
}

func promptFor(prompts, aliases map[string]string, body []byte, fallback string) string {
	model := strings.TrimSpace(fallback)
	if bodyModel := gjson.GetBytes(body, "model"); bodyModel.Type == gjson.String && strings.TrimSpace(bodyModel.String()) != "" {
		model = strings.TrimSpace(bodyModel.String())
	}
	prompt := prompts[model]
	if next := strings.TrimSpace(aliases[model]); next != "" && next != model {
		if candidate := prompts[next]; candidate != "" {
			prompt = candidate
		}
	}
	return prompt
}

func requestModel(req pluginapi.RequestInterceptRequest) string {
	if model := strings.TrimSpace(req.RequestedModel); model != "" {
		return model
	}
	return strings.TrimSpace(req.Model)
}

func aliasHeader(result inject.Result) string {
	if result.AliasedFrom == "" || result.AliasedTo == "" {
		return ""
	}
	return result.AliasedFrom + " -> " + result.AliasedTo
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	switch value := metadata[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func splitQuery(path string, query map[string][]string) (string, map[string][]string) {
	path = strings.TrimSpace(path)
	if i := strings.IndexByte(path, '?'); i >= 0 {
		if query == nil {
			query = map[string][]string{}
		}
		for key, values := range parseQuery(path[i+1:]) {
			query[key] = append(query[key], values...)
		}
		path = path[:i]
	}
	if query == nil {
		query = map[string][]string{}
	}
	return path, query
}

func parseQuery(raw string) map[string][]string {
	out := map[string][]string{}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		key = queryUnescape(key)
		value = queryUnescape(value)
		if key == "" {
			continue
		}
		out[key] = append(out[key], value)
	}
	return out
}

func queryUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '+':
			b.WriteByte(' ')
		case '%':
			if i+2 < len(s) {
				if v, ok := hexByte(s[i+1], s[i+2]); ok {
					b.WriteByte(v)
					i += 2
					continue
				}
			}
			b.WriteByte('%')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func hexByte(a, b byte) (byte, bool) {
	hi, ok1 := fromHex(a)
	lo, ok2 := fromHex(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return hi<<4 | lo, true
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

type stamp struct {
	injected bool
	alias    string
	until    time.Time
}

func (st stamp) headers() http.Header {
	header := http.Header{}
	if st.injected {
		header.Set(inject.HeaderName, inject.HeaderValue)
	}
	if st.alias != "" {
		header.Set(inject.AliasHeaderName, st.alias)
	}
	if len(header) == 0 {
		return nil
	}
	return header
}

type markStore struct {
	mu     sync.Mutex
	byID   map[string]stamp
	byHash map[string]stamp
}

func newMarkStore() *markStore {
	return &markStore{byID: map[string]stamp{}, byHash: map[string]stamp{}}
}

func (m *markStore) put(id string, body []byte, injected bool, alias string) {
	if m == nil || (!injected && alias == "") {
		return
	}
	now := time.Now()
	item := stamp{injected: injected, alias: alias, until: now.Add(markTTL)}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(now)
	if id != "" {
		m.byID[id] = item
	}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		m.byHash[hex.EncodeToString(sum[:])] = item
	}
}

func (m *markStore) get(id string, body []byte) (stamp, bool) {
	if m == nil {
		return stamp{}, false
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != "" {
		if item, ok := m.byID[id]; ok && now.Before(item.until) {
			return item, true
		}
	}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		if item, ok := m.byHash[hex.EncodeToString(sum[:])]; ok && now.Before(item.until) {
			return item, true
		}
	}
	return stamp{}, false
}

func (m *markStore) sweepLocked(now time.Time) {
	if len(m.byID)+len(m.byHash) < maxMarkEntries {
		for id, item := range m.byID {
			if !now.Before(item.until) {
				delete(m.byID, id)
			}
		}
		for hash, item := range m.byHash {
			if !now.Before(item.until) {
				delete(m.byHash, hash)
			}
		}
		return
	}
	m.byID = map[string]stamp{}
	m.byHash = map[string]stamp{}
}

// Metadata is the registration block advertised to CPA.
func Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             pluginName,
		Version:          Version,
		Author:           pluginAuthor,
		GitHubRepository: pluginRepo,
		ConfigFields: []pluginapi.ConfigField{
			{Name: "data_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Directory for prompts, git clones, backups, and sync logs. Default: prompt-admin-data under the CPA working directory."},
			{Name: "cpa_url", Type: pluginapi.ConfigFieldTypeString, Description: "Base URL used to read GET /v1/models. Default: http://127.0.0.1:8317."},
			{Name: "cpa_config", Type: pluginapi.ConfigFieldTypeString, Description: "Optional CPA config.yaml. The first access api-key and alias map are read from it. The key is not logged or returned."},
			{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Optional advanced override for GET /v1/models only. Not required: the model list comes from cpa_config access.api-keys (or static models). Never logged or returned."},
			{Name: "names_file", Type: pluginapi.ConfigFieldTypeString, Description: "Optional JSON file of hidden aliases (old name to current name). Without it, alias rewrite is off."},
			{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Static model ids used when /v1/models and cpa_config yield none."},
			{Name: "watch_interval", Type: pluginapi.ConfigFieldTypeString, Description: "How often to re-read the model list. Default 15s. Integer means seconds."},
			{Name: "sync_interval", Type: pluginapi.ConfigFieldTypeString, Description: "How often to git-pull prompt repos. Default 1h. 0 disables the timer."},
			{Name: "allow_file_urls", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow file:// prompt repos. Leave false except in tests."},
		},
	}
}

// ManagementRegistration lists the embedded page and the authenticated actions.
func ManagementRegistration() ManagementRegistrationResponse {
	base := "/plugins/" + pluginID
	post := func(path, description string) ManagementRoute {
		return ManagementRoute{Method: http.MethodPost, Path: base + path, Description: description}
	}
	return ManagementRegistrationResponse{
		Routes: []ManagementRoute{
			{Method: http.MethodGet, Path: base + "/state", Description: "当前提示词、仓库和冲突快照"},
			{Method: http.MethodGet, Path: base + "/file", Description: "读取一个仓库文件的全文"},
			post("/sync", "立即拉取仓库并重新匹配"),
			post("/repos/add", "添加一个提示词仓库"),
			post("/repos/remove", "删除一个提示词仓库"),
			post("/repos/move", "调整仓库显示顺序"),
			post("/repos/update", "启用、停用、改子目录或 Token"),
			post("/choose", "在冲突候选中保留一份"),
			post("/override", "强制覆盖、指定文件或不注入"),
			post("/local", "保存或删除本地提示词"),
			post("/default", "保存默认提示词"),
		},
		Resources: []ResourceRoute{{
			Path:        "/index.html",
			Menu:        "模型提示词",
			Description: "按模型管理并注入系统提示词",
		}},
	}
}

// ManagementRegistrationResponse mirrors the host registration JSON.
type ManagementRegistrationResponse struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// ManagementRoute is one authenticated management path.
type ManagementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

// ResourceRoute is the browser page embedded in the management center.
type ResourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

// PluginID is the library basename and config key.
func PluginID() string { return pluginID }
