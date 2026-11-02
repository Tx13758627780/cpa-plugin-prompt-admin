// Package panel serves the Chinese management page and its JSON actions.
// The page is a CPA resource. Actions are management routes and require the CPA management key.
package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	_ "embed"

	"cpa-plugin-prompt-admin/internal/engine"
)

const PluginID = "prompt-admin"

//go:embed index.html
var indexHTML []byte

// Result is a management or resource HTTP response.
type Result struct {
	Status int
	Header http.Header
	Body   []byte
}

// Handle dispatches one plugin management or resource request.
func Handle(eng *engine.Engine, method, path string, query map[string][]string, body []byte) Result {
	if eng == nil {
		return jsonResult(http.StatusServiceUnavailable, map[string]any{"error": "plugin is not ready"})
	}
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch {
	case strings.HasSuffix(path, "/index.html") || strings.HasSuffix(path, "/prompt-admin"):
		if !strings.EqualFold(method, http.MethodGet) {
			return jsonResult(http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
		return Result{
			Status: http.StatusOK,
			Header: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-store"},
				"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'"},
				"Referrer-Policy":         {"no-referrer"},
				"X-Content-Type-Options":  {"nosniff"},
			},
			Body: indexHTML,
		}
	case strings.HasSuffix(path, "/state"):
		if !strings.EqualFold(method, http.MethodGet) {
			return jsonResult(http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
		return jsonResult(http.StatusOK, eng.Snapshot())
	case strings.HasSuffix(path, "/file"):
		if !strings.EqualFold(method, http.MethodGet) {
			return jsonResult(http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
		key := first(query["key"])
		text, ok := eng.ReadFile(key)
		if !ok {
			return jsonResult(http.StatusNotFound, map[string]any{"key": key, "text": nil})
		}
		return jsonResult(http.StatusOK, map[string]any{"key": key, "text": text})
	}
	if !strings.EqualFold(method, http.MethodPost) {
		return jsonResult(http.StatusNotFound, map[string]any{"error": "not found"})
	}
	if len(body) > 2_000_000 {
		return jsonResult(http.StatusRequestEntityTooLarge, map[string]any{"error": "请求太大"})
	}
	action := lastSegment(path)
	payload, err := decodeObject(body)
	if err != nil {
		return jsonResult(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	switch action {
	case "sync":
		ok, msg := eng.ManualSync()
		return jsonResult(http.StatusOK, map[string]any{"ok": true, "started": ok, "msg": msg})
	case "add":
		if err := eng.AddRepo(str(payload, "url"), str(payload, "branch"), str(payload, "subpath"), str(payload, "token")); err != nil {
			return actionError(err)
		}
		return jsonResult(http.StatusOK, map[string]any{"ok": true, "msg": "已保存，后台同步中"})
	case "remove":
		return actionResult(eng.RemoveRepo(str(payload, "id")), "已删除")
	case "move":
		return actionResult(eng.MoveRepo(str(payload, "id"), str(payload, "dir")), "已保存")
	case "update":
		var enabled *bool
		if _, ok := payload["enabled"]; ok {
			value := truthy(payload["enabled"])
			enabled = &value
		}
		var subpath *string
		if value, ok := payload["subpath"]; ok {
			text := asString(value)
			subpath = &text
		}
		var token *string
		if value, ok := payload["token"]; ok {
			text := asString(value)
			token = &text
		}
		if err := eng.UpdateRepo(str(payload, "id"), enabled, subpath, token); err != nil {
			return actionError(err)
		}
		msg := "已保存"
		if token != nil {
			msg = "已保存，后台同步中"
		}
		return jsonResult(http.StatusOK, map[string]any{"ok": true, "msg": msg})
	case "choose":
		return actionResult(eng.Choose(str(payload, "model"), str(payload, "key")), "已保留，并记住此选择")
	case "override":
		return actionResult(eng.Override(str(payload, "model"), str(payload, "type"), str(payload, "text"), str(payload, "key")), "已保存")
	case "local":
		return actionResult(eng.SetLocal(str(payload, "model"), str(payload, "text")), "已保存")
	case "default":
		return actionResult(eng.SetDefault(str(payload, "text")), "已保存")
	default:
		return jsonResult(http.StatusNotFound, map[string]any{"error": "未知操作"})
	}
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func actionResult(err error, msg string) Result {
	if err != nil {
		return actionError(err)
	}
	return jsonResult(http.StatusOK, map[string]any{"ok": true, "msg": msg})
}

func actionError(err error) Result {
	if _, ok := err.(engine.UserError); ok {
		return jsonResult(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	return jsonResult(http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

func jsonResult(status int, value any) Result {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return Result{Status: http.StatusInternalServerError, Header: jsonHeader(), Body: []byte("{\"error\":\"encode failed\"}\n")}
	}
	return Result{Status: status, Header: jsonHeader(), Body: buf.Bytes()}
}

func jsonHeader() http.Header {
	return http.Header{
		"Content-Type":           {"application/json; charset=utf-8"},
		"Cache-Control":          {"no-store"},
		"X-Content-Type-Options": {"nosniff"},
	}
}

func decodeObject(body []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, engine.UserError("请求不是 JSON 对象")
	}
	if obj == nil {
		obj = map[string]any{}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, engine.UserError("请求不是 JSON 对象")
	}
	return obj, nil
}

func str(obj map[string]any, key string) string {
	return asString(obj[key])
}

func asString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func truthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	default:
		return false
	}
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
