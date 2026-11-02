// Package match ports the prompt-admin deterministic filename scorer.
// Scores are string comparisons only. No model is called.
package match

import (
	"regexp"
	"strings"
)

const MinScore = 40

// Families and aliases match the reference prompt-admin scorer.
var (
	Families = []string{
		"claude", "gpt", "chatgpt", "codex", "gemini", "gemma", "llama", "mistral", "mixtral",
		"codestral", "magistral", "devstral", "ministral", "deepseek", "qwen", "qwq", "nemotron",
		"grok", "kimi", "moonshot", "glm", "chatglm", "phi", "command", "cohere", "minimax",
		"hunyuan", "doubao", "ernie", "yi", "baichuan", "internlm", "granite", "olmo", "jamba",
		"nova", "sonar", "perplexity", "copilot", "cursor", "inkling", "lfm", "hermes", "dots",
	}
	familyAlias = map[string]string{
		"chatgpt": "gpt", "openai": "gpt", "moonshot": "kimi", "chatglm": "glm",
		"cohere": "command", "anthropic": "claude", "bard": "gemini", "xai": "grok",
	}
	stopTokens = map[string]struct{}{
		"free": {}, "latest": {}, "it": {}, "instruct": {}, "chat": {}, "preview": {}, "exp": {}, "beta": {}, "groq": {},
		"openrouter": {}, "or": {}, "model": {}, "models": {}, "the": {}, "and": {}, "for": {}, "auto": {}, "v": {}, "vl": {},
		"system": {}, "prompt": {}, "prompts": {}, "md": {}, "txt": {}, "json": {}, "yaml": {}, "yml": {},
	}
	coreStrip = regexp.MustCompile(`(systemprompts?|systemmessage|prompts?|instructions?|system)`)
	tokenRE   = regexp.MustCompile(`[a-z]+|[0-9]+[a-z]*`)
	familySet = func() map[string]struct{} {
		out := make(map[string]struct{}, len(Families))
		for _, f := range Families {
			out[f] = struct{}{}
		}
		return out
	}()
)

// Profile is the normalized view of one model id.
type Profile struct {
	Name    string
	Norm    string
	Compact string
	Tokens  []string
	Family  string
}

// FileProfile is the normalized view of one repo-relative path.
type FileProfile struct {
	FCompact string
	FCore    string
	PCompact string
	FTokens  map[string]struct{}
	PTokens  map[string]struct{}
	FFam     map[string]struct{}
	PFam     map[string]struct{}
}

// Tokenize splits a lowercased string into word and number tokens.
func Tokenize(s string) []string {
	return tokenRE.FindAllString(strings.ToLower(s), -1)
}

// Compact removes every character that is not an ASCII letter or digit.
func Compact(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NormModel lowercases a model id, drops a :suffix, a vendor prefix, and a file extension.
func NormModel(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.IndexByte(n, ':'); i >= 0 {
		n = n[:i]
	}
	if i := strings.LastIndexByte(n, '/'); i >= 0 {
		n = n[i+1:]
	}
	n = extRE.ReplaceAllString(n, "")
	return n
}

var extRE = regexp.MustCompile(`\.(md|txt|json|ya?ml|gguf)$`)

// ModelProfile builds a scorer profile for a model id.
func ModelProfile(name string) Profile {
	n := NormModel(name)
	toks := Tokenize(n)
	meaningful := make([]string, 0, len(toks))
	for _, t := range toks {
		if _, stop := stopTokens[t]; stop || isDigits(t) || len(t) < 2 {
			continue
		}
		meaningful = append(meaningful, t)
	}
	fam := ""
	for _, t := range toks {
		t2 := t
		if a, ok := familyAlias[t]; ok {
			t2 = a
		}
		if _, ok := familySet[t2]; ok {
			fam = t2
			break
		}
	}
	return Profile{Name: name, Norm: n, Compact: Compact(n), Tokens: meaningful, Family: fam}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FileProfileOf builds a scorer profile for a repo-relative path.
func FileProfileOf(path string) FileProfile {
	// Python os.path.splitext strips only the final extension.
	stemPath := strings.TrimSuffix(strings.ToLower(path), lastExt(path))
	stem := stemPath
	if i := strings.LastIndex(stemPath, "/"); i >= 0 {
		stem = stemPath[i+1:]
	}
	fc := Compact(stem)
	ftoks := toSet(Tokenize(stem))
	ptoks := toSet(Tokenize(stemPath))
	return FileProfile{
		FCompact: fc,
		FCore:    coreStrip.ReplaceAllString(fc, ""),
		PCompact: Compact(stemPath),
		FTokens:  ftoks,
		PTokens:  ptoks,
		FFam:     familiesOf(ftoks),
		PFam:     familiesOf(ptoks),
	}
}

func lastExt(path string) string {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		return strings.ToLower(base[i:])
	}
	return ""
}

func toSet(items []string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, item := range items {
		out[item] = struct{}{}
	}
	return out
}

func familiesOf(toks map[string]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for t := range toks {
		t2 := t
		if a, ok := familyAlias[t]; ok {
			t2 = a
		}
		if _, ok := familySet[t2]; ok {
			out[t2] = struct{}{}
		}
	}
	return out
}

// ScoreOne scores one model profile against one file profile.
// The reason strings are the Chinese labels shown in the management page.
func ScoreOne(mp Profile, fp FileProfile) (int, string) {
	c := mp.Compact
	if c != "" && (fp.FCompact == c || (fp.FCore != "" && fp.FCore == c)) {
		return 100, "文件名完全一致"
	}
	if len(c) >= 4 && strings.Contains(fp.FCompact, c) {
		return 90, "文件名包含模型名"
	}
	if len(c) >= 4 && strings.Contains(fp.PCompact, c) {
		return 85, "路径包含模型名"
	}
	toks := mp.Tokens
	if len(toks) > 0 && anyLen3(toks) {
		if allIn(toks, fp.FTokens) {
			return 80, "文件名包含全部关键词 " + strings.Join(toks, "+")
		}
		if allIn(toks, fp.PTokens) {
			return 70, "路径包含全部关键词 " + strings.Join(toks, "+")
		}
	}
	if mp.Family != "" {
		if _, ok := fp.FFam[mp.Family]; ok {
			return 50, "同系列（" + mp.Family + "）文件名"
		}
		if _, ok := fp.PFam[mp.Family]; ok {
			return 40, "同系列（" + mp.Family + "）目录"
		}
	}
	return 0, ""
}

func anyLen3(toks []string) bool {
	for _, t := range toks {
		if len(t) >= 3 {
			return true
		}
	}
	return false
}

func allIn(toks []string, set map[string]struct{}) bool {
	for _, t := range toks {
		if _, ok := set[t]; !ok {
			return false
		}
	}
	return true
}

// ScoreModel scores the exposed model id and, when present, its upstream id.
// Upstream hits below 70 are ignored. Qualifying upstream hits lose 5 points.
func ScoreModel(model, upstream string, fp FileProfile, cache map[string]Profile) (int, string) {
	names := []string{model}
	if upstream != "" && upstream != model {
		names = append(names, upstream)
	}
	bestS, bestWhy := 0, ""
	for i, name := range names {
		mp, ok := cache[name]
		if !ok {
			mp = ModelProfile(name)
			if cache != nil {
				cache[name] = mp
			}
		}
		s, why := ScoreOne(mp, fp)
		if i == 1 && s > 0 {
			if s < 70 {
				continue
			}
			s, why = s-5, why+"（上游 "+upstream+"）"
		}
		if s > bestS {
			bestS, bestWhy = s, why
		}
	}
	return bestS, bestWhy
}
