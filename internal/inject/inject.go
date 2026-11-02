// Package inject prepends a per-model system prompt onto inbound CPA payloads.
// Gateway text is placed first. An existing client system message stays after it.
package inject

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// HeaderName is the downstream response header set when a prompt is in effect.
	HeaderName = "X-Prompt-Admin"
	// HeaderValue is the header value sent with HeaderName.
	HeaderValue = "injected"
	// AliasHeaderName reports a hidden old model name rewritten to the current id.
	AliasHeaderName = "X-Model-Alias"
	// NestedSource is CPA metadata for a host.model callback. Those requests are not injected again.
	NestedSource = "plugin_host_model_callback"
)

// Request is one inbound execution payload.
type Request struct {
	SourceFormat string
	Model        string
	Body         []byte
	Prompt       string
	Aliases      map[string]string
	RequestPath  string
}

// Result is the payload after alias rewrite and prompt injection.
type Result struct {
	Body        []byte
	Changed     bool
	Injected    bool
	AliasedFrom string
	AliasedTo   string
}

// Apply rewrites model aliases, then prepends the prompt for the effective model.
// A prompt that is already the leading system message is left in place and still
// reported as injected, so retries do not stack a second copy.
func Apply(req Request) Result {
	out := Result{Body: req.Body}
	if skipPath(req.RequestPath) || len(req.Body) == 0 || !gjson.ValidBytes(req.Body) || !gjson.GetBytes(req.Body, "@this").IsObject() {
		return out
	}
	model := strings.TrimSpace(req.Model)
	if bodyModel := gjson.GetBytes(req.Body, "model"); bodyModel.Type == gjson.String && strings.TrimSpace(bodyModel.String()) != "" {
		model = strings.TrimSpace(bodyModel.String())
	}
	if model == "" {
		return out
	}
	if next := strings.TrimSpace(req.Aliases[model]); next != "" && next != model {
		updated, err := sjson.SetBytes(req.Body, "model", next)
		if err != nil {
			return out
		}
		out.Body = updated
		out.Changed = true
		out.AliasedFrom = model
		out.AliasedTo = next
	}
	prompt := req.Prompt
	if prompt == "" {
		return out
	}
	format := canonicalFormat(req.SourceFormat, out.Body)
	updated, injected := injectFormat(format, out.Body, prompt, req.RequestPath)
	if injected {
		out.Injected = true
	}
	if updated != nil && !bytes.Equal(updated, out.Body) {
		out.Body = updated
		out.Changed = true
	}
	return out
}

func injectFormat(format string, body []byte, prompt, path string) ([]byte, bool) {
	switch format {
	case "openai":
		return injectOpenAI(body, prompt, path)
	case "openai-response":
		return injectInstructions(body, "instructions", prompt)
	case "claude":
		return injectClaude(body, prompt)
	case "gemini":
		return injectGemini(body, prompt)
	case "interactions":
		return injectInteractions(body, prompt)
	default:
		return body, false
	}
}

func canonicalFormat(source string, body []byte) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "openai", "chat-completions", "chat_completions":
		return "openai"
	case "openai-response", "responses", "response":
		return "openai-response"
	case "claude":
		return "claude"
	case "gemini":
		return "gemini"
	case "interactions", "gemini-cli":
		return "interactions"
	}
	if gjson.GetBytes(body, "contents").Exists() || gjson.GetBytes(body, "systemInstruction").Exists() {
		return "gemini"
	}
	if gjson.GetBytes(body, "system").Exists() && gjson.GetBytes(body, "messages").Exists() {
		return "claude"
	}
	if gjson.GetBytes(body, "messages").Exists() {
		return "openai"
	}
	if gjson.GetBytes(body, "instructions").Exists() || gjson.GetBytes(body, "input").Exists() {
		return "openai-response"
	}
	if gjson.GetBytes(body, "system_instruction").Exists() {
		return "interactions"
	}
	return ""
}

func skipPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	for _, suffix := range []string{
		"/v1/images/generations", "/v1/images/edits",
		"/images/generations", "/images/edits",
		"/v1/responses/compact", "/responses/compact",
		"/v1/embeddings", "/embeddings",
	} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func isChatPath(path string) bool {
	path = strings.TrimSpace(path)
	return strings.HasSuffix(path, "/chat/completions") || path == "/chat/completions"
}

func injectOpenAI(body []byte, prompt, path string) ([]byte, bool) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || messages.Type == gjson.Null {
		if !isChatPath(path) && path != "" {
			return body, false
		}
		// Chat completions with no messages array still receive the gateway system message.
		if path == "" && !messages.Exists() {
			return body, false
		}
		raw, err := marshalJSON(map[string]any{"role": "system", "content": prompt})
		if err != nil {
			return body, false
		}
		updated, err := sjson.SetRawBytes(body, "messages", append(append([]byte{'['}, raw...), ']'))
		if err != nil {
			return body, false
		}
		return updated, true
	}
	if !messages.IsArray() {
		return body, false
	}
	if leadingSystemEquals(messages, prompt) {
		return body, true
	}
	raw, err := marshalJSON(map[string]any{"role": "system", "content": prompt})
	if err != nil {
		return body, false
	}
	return prependArrayElement(body, "messages", raw), true
}

func leadingSystemEquals(messages gjson.Result, prompt string) bool {
	items := messages.Array()
	if len(items) == 0 {
		return false
	}
	first := items[0]
	if first.Get("role").String() != "system" {
		return false
	}
	return contentEquals(first.Get("content"), prompt)
}

func contentEquals(content gjson.Result, prompt string) bool {
	if content.Type == gjson.String {
		return content.String() == prompt
	}
	if !content.IsArray() {
		return false
	}
	parts := content.Array()
	if len(parts) != 1 {
		return false
	}
	part := parts[0]
	kind := part.Get("type").String()
	if kind != "" && kind != "text" && kind != "input_text" {
		return false
	}
	return part.Get("text").String() == prompt
}

func injectInstructions(body []byte, field, prompt string) ([]byte, bool) {
	current := gjson.GetBytes(body, field)
	if !current.Exists() || current.Type == gjson.Null || (current.Type == gjson.String && current.String() == "") {
		updated, err := sjson.SetBytes(body, field, prompt)
		if err != nil {
			return body, false
		}
		return updated, true
	}
	if current.Type != gjson.String {
		return body, false
	}
	text := current.String()
	if text == prompt || strings.HasPrefix(text, prompt+"\n\n") {
		return body, true
	}
	updated, err := sjson.SetBytes(body, field, prompt+"\n\n"+text)
	if err != nil {
		return body, false
	}
	return updated, true
}

func injectClaude(body []byte, prompt string) ([]byte, bool) {
	system := gjson.GetBytes(body, "system")
	if !system.Exists() || system.Type == gjson.Null {
		updated, err := sjson.SetBytes(body, "system", prompt)
		if err != nil {
			return body, false
		}
		return updated, true
	}
	if system.Type == gjson.String {
		return injectInstructions(body, "system", prompt)
	}
	if system.IsArray() {
		return prependTextBlock(body, "system", prompt, map[string]any{"type": "text", "text": prompt})
	}
	return body, false
}

func injectGemini(body []byte, prompt string) ([]byte, bool) {
	field := "systemInstruction"
	if !gjson.GetBytes(body, "systemInstruction").Exists() && gjson.GetBytes(body, "system_instruction").Exists() {
		field = "system_instruction"
	}
	current := gjson.GetBytes(body, field)
	if !current.Exists() || current.Type == gjson.Null {
		raw, err := marshalJSON(map[string]any{"parts": []map[string]string{{"text": prompt}}})
		if err != nil {
			return body, false
		}
		updated, err := sjson.SetRawBytes(body, field, raw)
		if err != nil {
			return body, false
		}
		return updated, true
	}
	parts := gjson.GetBytes(body, field+".parts")
	if parts.IsArray() {
		return prependTextBlock(body, field+".parts", prompt, map[string]any{"text": prompt})
	}
	return body, false
}

func injectInteractions(body []byte, prompt string) ([]byte, bool) {
	current := gjson.GetBytes(body, "system_instruction")
	if !current.Exists() || current.Type == gjson.Null {
		updated, err := sjson.SetBytes(body, "system_instruction", prompt)
		if err != nil {
			return body, false
		}
		return updated, true
	}
	if current.Type == gjson.String {
		return injectInstructions(body, "system_instruction", prompt)
	}
	if text := current.Get("text"); text.Type == gjson.String {
		return injectInstructions(body, "system_instruction.text", prompt)
	}
	parts := current.Get("parts")
	if parts.IsArray() {
		return prependTextBlock(body, "system_instruction.parts", prompt, map[string]any{"text": prompt})
	}
	return body, false
}

func prependTextBlock(body []byte, path, prompt string, block map[string]any) ([]byte, bool) {
	array := gjson.GetBytes(body, path)
	if array.IsArray() && len(array.Array()) > 0 {
		first := array.Array()[0]
		if first.Get("text").String() == prompt || first.Type == gjson.String && first.String() == prompt {
			return body, true
		}
	}
	raw, err := marshalJSON(block)
	if err != nil {
		return body, false
	}
	if !array.Exists() || array.Type == gjson.Null {
		updated, err := sjson.SetRawBytes(body, path, append(append([]byte{'['}, raw...), ']'))
		if err != nil {
			return body, false
		}
		return updated, true
	}
	if !array.IsArray() {
		return body, false
	}
	return prependArrayElement(body, path, raw), true
}

func prependArrayElement(payload []byte, path string, raw []byte) []byte {
	array := gjson.GetBytes(payload, path)
	if !array.IsArray() {
		return payload
	}
	var buffer bytes.Buffer
	buffer.WriteByte('[')
	buffer.Write(raw)
	for _, item := range array.Array() {
		buffer.WriteByte(',')
		buffer.WriteString(item.Raw)
	}
	buffer.WriteByte(']')
	updated, err := sjson.SetRawBytes(payload, path, buffer.Bytes())
	if err != nil {
		return payload
	}
	return updated
}

func marshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
