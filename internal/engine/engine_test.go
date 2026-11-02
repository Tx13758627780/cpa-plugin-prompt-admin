package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"cpa-plugin-prompt-admin/internal/config"
)

func TestParseSourceGitHubTreeAndBlob(t *testing.T) {
	repo, err := parseSource("https://github.com/example/prompts/tree/main/system", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if repo.URL != "https://github.com/example/prompts" || repo.Branch != "main" || repo.Subpath != "system" || repo.Kind != "git" {
		t.Fatalf("tree = %+v", repo)
	}
	raw, err := parseSource("acme/prompts", "", "", false)
	if err != nil || raw.URL != "https://github.com/acme/prompts" {
		t.Fatalf("shorthand = %+v %v", raw, err)
	}
	blob, err := parseSource("https://github.com/acme/prompts/blob/main/gemini-2.5-flash.md", "", "", false)
	if err != nil || blob.Kind != "raw" || blob.URL != "https://raw.githubusercontent.com/acme/prompts/main/gemini-2.5-flash.md" {
		t.Fatalf("blob = %+v %v", blob, err)
	}
	if _, err := parseSource("file:///tmp/prompts", "", "", false); err == nil {
		t.Fatal("file URL accepted without the test flag")
	}
	if _, err := parseSource("https://github.com/acme/prompts", "evil branch", "", false); err == nil {
		t.Fatal("branch with a space was accepted")
	}
}

func TestConflictKeepsPreviousUntilChoice(t *testing.T) {
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(origin, "gemini-2.5-flash.md"), "FROM-A\n")
	writeFile(t, filepath.Join(origin, "gemini-2.5-flash-system-prompt.md"), "FROM-B\n")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=prompt-admin", "GIT_AUTHOR_EMAIL=prompt-admin@example.com", "GIT_COMMITTER_NAME=prompt-admin", "GIT_COMMITTER_EMAIL=prompt-admin@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("add", ".")
	git("commit", "-m", "prompts")

	data := filepath.Join(dir, "data")
	cfg, err := config.Parse([]byte("data_dir: " + data + "\nallow_file_urls: true\ncpa_url: http://127.0.0.1:9\nwatch_interval: 1s\nsync_interval: 0s\n"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(data, "prompts.json"), "{\n  \"gemini-2.5-flash\": \"KEEP\"\n}\n")
	eng, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	eng.fetch = func() modelSnap {
		return modelSnap{IDs: []string{"gemini-2.5-flash"}, Upstream: map[string]string{}, Source: "test"}
	}
	repo, err := parseSource("file://"+origin, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	repo.Enabled = &enabled
	eng.state.Local = map[string]string{"removed-model": "still here"}
	eng.state.HasLast = true
	eng.state.LastOutput = map[string]string{"gemini-2.5-flash": "KEEP"}
	eng.state.Repos = []Repo{repo}
	if err := eng.computeBody(true, "test"); err != nil {
		t.Fatal(err)
	}
	if eng.status.Rows[0].Source != "conflict" {
		t.Fatalf("source = %s, want conflict (%d candidates)", eng.status.Rows[0].Source, len(eng.status.Rows[0].Candidates))
	}
	if eng.status.Rows[0].Prompt != "KEEP" {
		t.Fatalf("prompt = %q, want previous KEEP", eng.status.Rows[0].Prompt)
	}
	prompts, _ := eng.Prompts()
	if prompts["gemini-2.5-flash"] != "KEEP" {
		t.Fatalf("resolved map = %v", prompts)
	}
	if _, ok := prompts["removed-model"]; ok {
		t.Fatal("removed model was injected")
	}
	foundStored := false
	for _, name := range eng.status.StoredOnly {
		if name == "removed-model" {
			foundStored = true
		}
	}
	if !foundStored {
		t.Fatalf("stored_only = %v", eng.status.StoredOnly)
	}
	key := eng.status.Conflicts[0].Candidates[0].Key
	if err := eng.Choose("gemini-2.5-flash", key); err != nil {
		t.Fatal(err)
	}
	if eng.status.Rows[0].Source != "chosen" {
		t.Fatalf("after choose source = %s", eng.status.Rows[0].Source)
	}
	if eng.status.Rows[0].Prompt != "FROM-A" && eng.status.Rows[0].Prompt != "FROM-B" {
		t.Fatalf("chosen prompt = %q", eng.status.Rows[0].Prompt)
	}
	// A new equal-score file reopens the conflict and keeps the chosen text.
	writeFile(t, filepath.Join(origin, "gemini-2.5-flash-prompt.md"), "FROM-C\n")
	git("add", ".")
	git("commit", "-m", "another")
	if err := eng.computeBody(true, "again"); err != nil {
		t.Fatal(err)
	}
	if eng.status.Rows[0].Source != "conflict" {
		t.Fatalf("new candidate source = %s", eng.status.Rows[0].Source)
	}
	if eng.status.Rows[0].Prompt == "" || eng.status.Rows[0].Prompt == "FROM-C" {
		t.Fatalf("conflict did not keep the previous prompt: %q", eng.status.Rows[0].Prompt)
	}
}

func TestSingleFileAndDefault(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	cfg, err := config.Parse([]byte("data_dir: " + data + "\nallow_file_urls: true\nwatch_interval: 1s\nsync_interval: 0s\n"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	eng.fetch = func() modelSnap {
		return modelSnap{IDs: []string{"claude-opus-4", "unknown-model"}, Source: "test", Upstream: map[string]string{}}
	}
	repo, err := parseSource("file://"+filepath.Join(dir, "repo"), "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	repo.Enabled = &enabled
	writeFile(t, filepath.Join(eng.reposDir(), repo.ID, "claude", "policy.md"), "family prompt\n")
	eng.state.Repos = []Repo{repo}
	eng.state.DefaultPrompt = "DEFAULT"
	if err := eng.computeBody(false, "apply"); err != nil {
		t.Fatal(err)
	}
	byModel := map[string]Row{}
	for _, row := range eng.status.Rows {
		byModel[row.Model] = row
	}
	if byModel["claude-opus-4"].Source != "repo" || byModel["claude-opus-4"].Score == nil || *byModel["claude-opus-4"].Score != 40 {
		t.Fatalf("claude row = %+v", byModel["claude-opus-4"])
	}
	if byModel["unknown-model"].Source != "default" || byModel["unknown-model"].Prompt != "DEFAULT" {
		t.Fatalf("default row = %+v", byModel["unknown-model"])
	}
	if err := eng.Override("unknown-model", "none", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, row := range eng.status.Rows {
		if row.Model == "unknown-model" && (row.Source != "override-none" || row.Prompt != "") {
			t.Fatalf("override none = %+v", row)
		}
	}
}

func TestIdenticalLocalIsNotAConflict(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoDir, "gemini-2.5-flash.md"), "same text")
	cfg, err := config.Parse([]byte("data_dir: " + filepath.Join(dir, "data") + "\nallow_file_urls: true\nwatch_interval: 1s\nsync_interval: 0s\n"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	eng.fetch = func() modelSnap {
		return modelSnap{IDs: []string{"gemini-2.5-flash"}, Source: "test", Upstream: map[string]string{}}
	}
	repo, err := parseSource("file://"+repoDir, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	repo.Enabled = &enabled
	writeFile(t, filepath.Join(eng.reposDir(), repo.ID, "gemini-2.5-flash.md"), "same text")
	eng.state.Repos = []Repo{repo}
	eng.state.HasLast = true
	eng.state.Local = map[string]string{"gemini-2.5-flash": "same text"}
	if err := eng.computeBody(false, "apply"); err != nil {
		t.Fatal(err)
	}
	if eng.status.Rows[0].Source == "conflict" || len(eng.status.Rows[0].Candidates) != 1 {
		t.Fatalf("identical local became a conflict: %+v", eng.status.Rows[0])
	}
	if eng.status.Rows[0].Prompt != "same text" {
		t.Fatalf("prompt = %q", eng.status.Rows[0].Prompt)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
