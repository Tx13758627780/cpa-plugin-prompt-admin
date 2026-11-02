package inject

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestOpenAIPrependsBeforeClientSystem(t *testing.T) {
	body := []byte(`{"model":"mistral-small","messages":[{"role":"system","content":"client"},{"role":"user","content":"hi"}]}`)
	got := Apply(Request{SourceFormat: "openai", Model: "mistral-small", Body: body, Prompt: "gateway", RequestPath: "/v1/chat/completions"})
	if !got.Injected || !got.Changed {
		t.Fatalf("result = %+v", got)
	}
	msgs := gjson.GetBytes(got.Body, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if msgs[0].Get("content").String() != "gateway" || msgs[1].Get("content").String() != "client" {
		t.Fatalf("order = %s", got.Body)
	}
}

func TestOpenAIIdempotentRetry(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"gateway"},{"role":"user","content":"hi"}]}`)
	got := Apply(Request{SourceFormat: "openai", Body: body, Prompt: "gateway", RequestPath: "/v1/chat/completions"})
	if !got.Injected || got.Changed {
		t.Fatalf("retry should keep the existing gateway message: %+v", got)
	}
}

func TestSkipWhenNoPrompt(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	got := Apply(Request{SourceFormat: "openai", Body: body, RequestPath: "/v1/chat/completions"})
	if got.Injected || got.Changed || string(got.Body) != string(body) {
		t.Fatalf("empty prompt changed the body: %+v", got)
	}
}

func TestSkipNestedAndEmbeddings(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	got := Apply(Request{SourceFormat: "openai", Body: body, Prompt: "gateway", RequestPath: "/v1/embeddings"})
	if got.Injected {
		t.Fatal("embeddings were injected")
	}
}

func TestAliasRewriteThenPrompt(t *testing.T) {
	body := []byte(`{"model":"gemini-flash","messages":[{"role":"user","content":"hi"}]}`)
	got := Apply(Request{
		SourceFormat: "openai",
		Body:         body,
		Prompt:       "gateway",
		Aliases:      map[string]string{"gemini-flash": "gemini-2.5-flash"},
		RequestPath:  "/v1/chat/completions",
	})
	if got.AliasedFrom != "gemini-flash" || got.AliasedTo != "gemini-2.5-flash" {
		t.Fatalf("alias = %+v", got)
	}
	if gjson.GetBytes(got.Body, "model").String() != "gemini-2.5-flash" {
		t.Fatalf("model = %s", got.Body)
	}
	if gjson.GetBytes(got.Body, "messages.0.content").String() != "gateway" {
		t.Fatalf("prompt missing: %s", got.Body)
	}
}

func TestResponsesClaudeGemini(t *testing.T) {
	resp := Apply(Request{SourceFormat: "openai-response", Body: []byte(`{"model":"m","instructions":"client","input":"hi"}`), Prompt: "gateway"})
	if gjson.GetBytes(resp.Body, "instructions").String() != "gateway\n\nclient" {
		t.Fatalf("responses = %s", resp.Body)
	}
	again := Apply(Request{SourceFormat: "openai-response", Body: resp.Body, Prompt: "gateway"})
	if again.Changed {
		t.Fatal("responses retry stacked the prompt")
	}

	claude := Apply(Request{SourceFormat: "claude", Body: []byte(`{"model":"m","system":[{"type":"text","text":"client"}],"messages":[{"role":"user","content":"hi"}]}`), Prompt: "gateway"})
	if gjson.GetBytes(claude.Body, "system.0.text").String() != "gateway" || gjson.GetBytes(claude.Body, "system.1.text").String() != "client" {
		t.Fatalf("claude = %s", claude.Body)
	}

	gem := Apply(Request{SourceFormat: "gemini", Body: []byte(`{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`), Prompt: "gateway"})
	if gjson.GetBytes(gem.Body, "systemInstruction.parts.0.text").String() != "gateway" {
		t.Fatalf("gemini = %s", gem.Body)
	}

	inter := Apply(Request{SourceFormat: "interactions", Body: []byte(`{"model":"m","input":"hi"}`), Prompt: "gateway"})
	if gjson.GetBytes(inter.Body, "system_instruction").String() != "gateway" {
		t.Fatalf("interactions = %s", inter.Body)
	}
}

func TestMultipartAlreadyInjected(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"gateway"}]},{"role":"user","content":"hi"}]}`)
	got := Apply(Request{SourceFormat: "chat-completions", Body: body, Prompt: "gateway", RequestPath: "/v1/chat/completions"})
	if !got.Injected || got.Changed {
		t.Fatalf("multipart idempotency failed: changed=%v body=%s", got.Changed, got.Body)
	}
	if strings.Count(string(got.Body), "gateway") != 1 {
		t.Fatalf("body = %s", got.Body)
	}
}
