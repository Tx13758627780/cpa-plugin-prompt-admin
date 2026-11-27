package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cpa-plugin-prompt-admin/internal/config"
)

type archiveTransport func(*http.Request) (*http.Response, error)

func (f archiveTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type archiveEntry struct {
	name, body string
	mode       os.FileMode
}

func testArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func archiveResponse(r *http.Request, status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}
}
func archiveEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := Open(config.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}
func TestArchiveRequestsUseForgeDefaultsAndCredentials(t *testing.T) {
	for _, tc := range []struct{ url, branch, want, header, value string }{
		{"https://github.com/example/prompts", "", "https://api.github.com/repos/example/prompts/zipball", "Authorization", "Bearer private-token"},
		{"https://github.com/example/prompts", "feature/safe", "https://api.github.com/repos/example/prompts/zipball/feature%2Fsafe", "Authorization", "Bearer private-token"},
		{"https://gitlab.com/group/sub/prompts", "", "https://gitlab.com/api/v4/projects/group%2Fsub%2Fprompts/repository/archive.zip?include_lfs_blobs=false", "PRIVATE-TOKEN", "private-token"},
		{"https://gitlab.com/group/prompts", "feature/safe", "https://gitlab.com/api/v4/projects/group%2Fprompts/repository/archive.zip?include_lfs_blobs=false&sha=feature%2Fsafe", "PRIVATE-TOKEN", "private-token"},
		{"https://codeberg.org/example/prompts", "", "https://codeberg.org/api/v1/repos/example/prompts/archive/HEAD.zip", "Authorization", "token private-token"},
		{"https://codeberg.org/example/prompts", "feature/safe", "https://codeberg.org/api/v1/repos/example/prompts/archive/feature%2Fsafe.zip", "Authorization", "token private-token"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			req, err := archiveRequest(context.Background(), Repo{URL: tc.url, Branch: tc.branch, Token: "private-token"})
			if err != nil || req.URL.String() != tc.want || req.Header.Get(tc.header) != tc.value || strings.Contains(req.URL.String(), "private-token") {
				t.Fatalf("request %v %v", req, err)
			}
		})
	}
	req, err := archiveRequest(context.Background(), Repo{URL: "https://github.com/example/prompts"})
	if err != nil || req.Header.Get("Authorization") != "" || req.URL.String() != "https://github.com/example/prompts/archive/HEAD.zip" {
		t.Fatalf("public request %v %v", req, err)
	}
	if _, err := archiveRequest(context.Background(), Repo{URL: "https://forge.example.com/prompts/repo"}); err == nil || !strings.Contains(err.Error(), "找不到 git") {
		t.Fatalf("unsupported forge: %v", err)
	}
}

func TestNoGitArchiveSyncUpdatesPromptsAndRemovesOldFiles(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // Reproduce the minimal Docker image, not the CI runner.
	e := archiveEngine(t)
	repo, err := parseSource("example/prompts", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	e.state.Repos = []Repo{repo}
	e.fetch = func() modelSnap { return modelSnap{IDs: []string{"demo"}, Source: "test"} }
	body := testArchive(t, archiveEntry{name: "snapshot/demo.md", body: "FIRST"}, archiveEntry{name: "snapshot/old.txt", body: "old"}, archiveEntry{name: "snapshot/README.md", body: "skip"}, archiveEntry{name: "snapshot/.github/hidden.md", body: "skip"}, archiveEntry{name: "snapshot/node_modules/skip.md", body: "skip"}, archiveEntry{name: "snapshot/link.md", body: "../../outside", mode: os.ModeSymlink | 0777})
	e.client = &http.Client{Transport: archiveTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://github.com/example/prompts/archive/HEAD.zip" {
			t.Fatalf("URL %s", r.URL)
		}
		return archiveResponse(r, 200, body), nil
	})}
	if err := e.computeBody(true, "test:no-git"); err != nil {
		t.Fatal(err)
	}
	prompts, _ := e.Prompts()
	if prompts["demo"] != "FIRST" {
		t.Fatalf("prompts %v", prompts)
	}
	commit := e.status.Repos[repo.ID].Commit
	if !strings.HasPrefix(commit, "zip:") {
		t.Fatalf("snapshot id %s", commit)
	}
	if len(e.scanRepo(repo)) != 2 {
		t.Fatalf("files %+v", e.scanRepo(repo))
	}
	body = testArchive(t, archiveEntry{name: "next/demo.md", body: "SECOND"})
	if err := e.computeBody(true, "test:update"); err != nil {
		t.Fatal(err)
	}
	prompts, _ = e.Prompts()
	if prompts["demo"] != "SECOND" || e.status.Repos[repo.ID].Commit == commit {
		t.Fatalf("update %v", prompts)
	}
	if _, err := os.Stat(filepath.Join(e.reposDir(), repo.ID, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file retained: %v", err)
	}
	// A failed refresh keeps the previous prompt files and exposes the repo error.
	body = []byte("<html>Access login</html>")
	if err := e.computeBody(true, "test:failure"); err != nil {
		t.Fatal(err)
	}
	prompts, _ = e.Prompts()
	if prompts["demo"] != "SECOND" || !strings.Contains(e.status.Repos[repo.ID].Error, "有效 ZIP") {
		t.Fatalf("failed update: %v %+v", prompts, e.status.Repos[repo.ID])
	}
}

func TestArchiveFailurePreservesCacheAndRedactsResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
		length int64
		want   string
	}{
		{"401", 401, []byte(`{"error":"private-token"}`), 0, "HTTP 401"},
		{"403", 403, []byte("private-token"), 0, "HTTP 403"},
		{"404", 404, nil, 0, "HTTP 404"},
		{"HTML", 200, []byte("<html>private-token</html>"), 0, "有效 ZIP"},
		{"too large", 200, nil, maxArchiveBytes + 1, "64 MiB"},
		{"traversal", 200, testArchive(t, archiveEntry{name: "snapshot/../../escape.md", body: "BAD"}), 0, "不安全路径"},
		{"windows path", 200, testArchive(t, archiveEntry{name: "snapshot/C:/escape.md", body: "BAD"}), 0, "不安全路径"},
		{"backslash", 200, testArchive(t, archiveEntry{name: `snapshot/..\escape.md`, body: "BAD"}), 0, "不安全路径"},
		{"absolute", 200, testArchive(t, archiveEntry{name: "/escape.md", body: "BAD"}), 0, "不安全路径"},
		{"duplicate", 200, testArchive(t, archiveEntry{name: "snapshot/demo.md", body: "A"}, archiveEntry{name: "snapshot/demo.md", body: "B"}), 0, "重复文件"},
		{"no root", 200, testArchive(t, archiveEntry{name: "demo.md", body: "A"}), 0, "统一根目录"},
		{"mixed roots", 200, testArchive(t, archiveEntry{name: "a/demo.md", body: "A"}, archiveEntry{name: "b/demo.md", body: "B"}), 0, "统一根目录"},
		{"empty files", 200, testArchive(t, archiveEntry{name: "snapshot/README.md", body: "SKIP"}), 0, "没有可用的提示词"},
		{"oversized file", 200, testArchive(t, archiveEntry{name: "snapshot/demo.md", body: strings.Repeat("x", maxFileBytes+1)}), 0, "没有可用的提示词"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := archiveEngine(t)
			repo, _ := parseSource("example/prompts", "", "", false)
			repo.Token = "private-token"
			cache := filepath.Join(e.reposDir(), repo.ID, "demo.md")
			writeFile(t, cache, "KEEP")
			e.client = &http.Client{Transport: archiveTransport(func(r *http.Request) (*http.Response, error) {
				res := archiveResponse(r, tc.status, tc.body)
				if tc.length > 0 {
					res.ContentLength = tc.length
				}
				return res, nil
			})}
			_, err := e.pullRepoArchive(context.Background(), repo)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), repo.Token) {
				t.Fatalf("error %v", err)
			}
			data, readErr := os.ReadFile(cache)
			if readErr != nil || string(data) != "KEEP" {
				t.Fatalf("cache %q %v", data, readErr)
			}
			entries, _ := os.ReadDir(e.reposDir())
			if len(entries) != 1 {
				t.Fatalf("temporary files leaked: %v", entries)
			}
		})
	}
}

func TestArchiveSubpathAndRedirectCredentials(t *testing.T) {
	for _, repoURL := range []string{"https://github.com/example/prompts", "https://gitlab.com/group/prompts"} {
		t.Run(repoURL, func(t *testing.T) {
			e := archiveEngine(t)
			repo, _ := parseSource(repoURL, "", "prompts", false)
			repo.Token = "private-token"
			body := testArchive(t, archiveEntry{name: "snapshot/prompts/demo.md", body: "SUBPATH"})
			calls := 0
			e.client = &http.Client{Transport: archiveTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					res := archiveResponse(r, 302, nil)
					res.Header.Set("Location", "https://codeload.example.com/signed.zip?token=private-token")
					return res, nil
				}
				if r.Header.Get("Authorization") != "" || r.Header.Get("PRIVATE-TOKEN") != "" {
					t.Fatal("credential forwarded across origins")
				}
				return archiveResponse(r, 200, body), nil
			})}
			if _, err := e.pullRepoArchive(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
			if len(e.scanRepo(repo)) != 1 {
				t.Fatalf("subpath files %v", e.scanRepo(repo))
			}
			repo.Subpath = "missing"
			calls = 0
			if _, err := e.pullRepoArchive(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "子目录") {
				t.Fatalf("subpath error %v", err)
			}
		})
	}
}

func TestArchiveCancellationAndRawWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	e := archiveEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.client = &http.Client{Transport: archiveTransport(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return archiveResponse(r, 200, []byte("RAW PROMPT")), nil
	})}
	repo, _ := parseSource("example/prompts", "", "", false)
	if _, err := e.pullRepo(ctx, repo); err == nil {
		t.Fatal("cancelled archive request succeeded")
	}
	raw, _ := parseSource("https://raw.githubusercontent.com/example/prompts/main/demo.md", "", "", false)
	if _, err := e.pullRepo(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	files := e.scanRepo(raw)
	if len(files) != 1 || files[0].Path != "demo.md" {
		t.Fatalf("raw files %v", files)
	}
}

func TestArchiveCorruptCRCDoesNotReplaceCache(t *testing.T) {
	e := archiveEngine(t)
	repo, _ := parseSource("example/prompts", "", "", false)
	// Store without compression, then mutate the actual payload to invalidate CRC.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.CreateHeader(&zip.FileHeader{Name: "snapshot/demo.md", Method: zip.Store})
	fmt.Fprint(f, "UNIQUE_CONTENT")
	w.Close()
	body := append([]byte(nil), buf.Bytes()...)
	pos := bytes.Index(body, []byte("UNIQUE_CONTENT"))
	body[pos] = 'X'
	e.client = &http.Client{Transport: archiveTransport(func(r *http.Request) (*http.Response, error) { return archiveResponse(r, 200, body), nil })}
	cache := filepath.Join(e.reposDir(), repo.ID, "demo.md")
	writeFile(t, cache, "KEEP")
	if _, err := e.pullRepoArchive(context.Background(), repo); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	data, _ := os.ReadFile(cache)
	if string(data) != "KEEP" {
		t.Fatal("corrupt archive replaced cache")
	}
}

func TestFailedGitCloneKeepsExistingArchiveCache(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git needed to test migration from an archive cache")
	}
	e := archiveEngine(t)
	e.cfg.AllowFileURLs = true
	repo, err := parseSource("file://"+filepath.ToSlash(filepath.Join(t.TempDir(), "missing-repository")), "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(e.reposDir(), repo.ID, "demo.md")
	writeFile(t, cache, "KEEP")
	if _, err := e.pullRepo(context.Background(), repo); err == nil {
		t.Fatal("missing repo unexpectedly cloned")
	}
	data, err := os.ReadFile(cache)
	if err != nil || string(data) != "KEEP" {
		t.Fatalf("failed clone erased cache: %q %v", data, err)
	}
}
