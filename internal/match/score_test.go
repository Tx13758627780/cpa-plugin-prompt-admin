package match

import "testing"

func TestScoreTable(t *testing.T) {
	cases := []struct {
		model string
		path  string
		want  int
		why   string
	}{
		{"gemini-2.5-flash", "gemini-2.5-flash.md", 100, "文件名完全一致"},
		{"gemini-2.5-flash", "prompts/gemini-2.5-flash-system-prompt.md", 100, "文件名完全一致"},
		{"gemini-2.5-flash", "notes/gemini-2.5-flash-extra.md", 90, "文件名包含模型名"},
		{"gemini-2.5-flash", "vendor/gemini-2.5-flash/guide.md", 85, "路径包含模型名"},
		{"meta/llama-3.3-70b", "llama-70b-tips.md", 80, "文件名包含全部关键词 llama+70b"},
		{"meta/llama-3.3-70b", "docs/llama/70b/overview.md", 70, "路径包含全部关键词 llama+70b"},
		{"claude-opus-4", "claude-guide.md", 50, "同系列（claude）文件名"},
		{"claude-opus-4", "claude/policy.md", 40, "同系列（claude）目录"},
		{"claude-opus-4", "unrelated/policy.md", 0, ""},
	}
	for _, tc := range cases {
		s, why := ScoreModel(tc.model, "", FileProfileOf(tc.path), map[string]Profile{})
		if s != tc.want || why != tc.why {
			t.Errorf("%s vs %s = %d %q, want %d %q", tc.model, tc.path, s, why, tc.want, tc.why)
		}
	}
}

func TestUpstreamPenalty(t *testing.T) {
	cache := map[string]Profile{}
	s, why := ScoreModel("groq-llama70b", "openai/gpt-oss-20b", FileProfileOf("gpt-oss-20b.md"), cache)
	if s != 95 || why != "文件名完全一致（上游 openai/gpt-oss-20b）" {
		t.Fatalf("upstream exact = %d %q", s, why)
	}
	// Family-only upstream hits are ignored.
	s, why = ScoreModel("custom-router", "gpt-oss-20b", FileProfileOf("gpt.md"), cache)
	if s != 0 {
		t.Fatalf("loose upstream family = %d %q, want 0", s, why)
	}
}

func TestNormDropsVendorAndSuffix(t *testing.T) {
	p := ModelProfile("nvidia/nemotron-3-ultra:free")
	if p.Compact != "nemotron3ultra" {
		t.Fatalf("compact = %q", p.Compact)
	}
	if p.Family != "nemotron" {
		t.Fatalf("family = %q", p.Family)
	}
}

func TestMinScoreBoundary(t *testing.T) {
	s, _ := ScoreModel("claude-opus-4", "", FileProfileOf("claude/policy.md"), nil)
	if s < MinScore {
		t.Fatalf("family directory score %d is below the published minimum", s)
	}
}
