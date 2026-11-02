package engine

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxFileBytes = 256 * 1024
	maxFiles     = 20000
	maxPreview   = 6000
)

var (
	promptExt = map[string]struct{}{".md": {}, ".txt": {}, ".json": {}, ".yaml": {}, ".yml": {}}
	skipName  = regexp.MustCompile(`(?i)^(readme|license|licence|changelog|changes|contributing|code_of_conduct|security|funding|codeowners|authors|notice|package(-lock)?|tsconfig|composer|mkdocs|_config|\.)`)
	skipDirs  = map[string]struct{}{
		"node_modules": {}, ".git": {}, ".github": {}, "__pycache__": {}, "vendor": {}, "dist": {}, "build": {},
	}
)

type scannedFile struct {
	Path string
	Size int64
}

func (e *Engine) pullRepo(ctx context.Context, repo Repo) (string, error) {
	dir := filepath.Join(e.reposDir(), repo.ID)
	if repo.Kind == "raw" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, repo.URL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "prompt-admin")
		if repo.Token != "" {
			req.Header.Set("Authorization", "token "+repo.Token)
		}
		client := e.httpClient()
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", UserError("下载失败: HTTP " + resp.Status)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
		if err != nil {
			return "", err
		}
		if len(body) > maxFileBytes {
			return "", UserError("文件超过 256KB")
		}
		name := filepath.Base(req.URL.Path)
		if name == "" || name == "." || name == "/" {
			name = "prompt.txt"
		}
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			return "", err
		}
		sum := sha1.Sum(body)
		return hex.EncodeToString(sum[:])[:8], nil
	}
	env := e.gitEnv(repo.URL, repo.Token)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		_ = os.RemoveAll(dir)
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return "", err
		}
		args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags"}
		if repo.Branch != "" {
			args = append(args, "--branch", repo.Branch)
		}
		args = append(args, "--", repo.URL, dir)
		if err := runGit(ctx, args, env, ""); err != nil {
			return "", err
		}
	} else {
		ref := repo.Branch
		if ref == "" {
			ref = "HEAD"
		}
		if err := runGit(ctx, []string{"fetch", "--depth", "1", "--no-tags", "origin", ref}, env, dir); err != nil {
			return "", err
		}
		if err := runGit(ctx, []string{"reset", "--hard", "-q", "FETCH_HEAD"}, env, dir); err != nil {
			return "", err
		}
		if err := runGit(ctx, []string{"clean", "-fdxq"}, env, dir); err != nil {
			return "", err
		}
	}
	out, err := runGitOutput(ctx, []string{"rev-parse", "--short", "HEAD"}, env, dir)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (e *Engine) gitEnv(rawURL, token string) []string {
	protocols := "http:https"
	if e.cfg.AllowFileURLs {
		protocols = "http:https:file"
	}
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/true",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(e.dataDir, ".gitconfig-empty"),
		"GIT_ALLOW_PROTOCOL=" + protocols,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
	}
	if token == "" {
		return env
	}
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	user := "x-access-token"
	if strings.Contains(strings.ToLower(host), "gitlab") {
		user = "oauth2"
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://"+host+"/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+encoded,
	)
}

func runGit(ctx context.Context, args, env []string, dir string) error {
	_, err := runGitOutput(ctx, args, env, dir)
	return err
}

func runGitOutput(ctx context.Context, args, env []string, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		last := ""
		if len(lines) > 0 {
			last = strings.TrimSpace(lines[len(lines)-1])
		}
		if last == "" {
			last = err.Error()
		}
		verb := "git"
		if len(args) > 0 {
			verb = "git " + args[0]
		}
		return "", UserError(verb + " 失败: " + trimRunes(last, 300))
	}
	return string(out), nil
}

func (e *Engine) scanRepo(repo Repo) []scannedFile {
	base, err := filepath.Abs(filepath.Join(e.reposDir(), repo.ID))
	if err != nil {
		return nil
	}
	base = filepath.Clean(base)
	root := base
	if repo.Subpath != "" {
		root = filepath.Clean(filepath.Join(base, filepath.FromSlash(repo.Subpath)))
	}
	if !within(base, root) {
		return nil
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	var files []scannedFile
	if !info.IsDir() {
		if f, ok := acceptFile(base, root, info); ok {
			files = append(files, f)
		}
		return files
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || len(files) >= maxFiles {
			if len(files) >= maxFiles {
				return filepath.SkipAll
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || skipDir(name)) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if f, ok := acceptFile(base, path, info); ok {
			files = append(files, f)
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}
	return files
}

func skipDir(name string) bool {
	_, ok := skipDirs[name]
	return ok
}

func acceptFile(base, path string, info os.FileInfo) (scannedFile, bool) {
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return scannedFile{}, false
	}
	if !within(base, path) {
		return scannedFile{}, false
	}
	ext := strings.ToLower(filepath.Ext(path))
	if _, ok := promptExt[ext]; !ok || skipName.MatchString(filepath.Base(path)) {
		return scannedFile{}, false
	}
	if info.Size() == 0 || info.Size() > maxFileBytes {
		return scannedFile{}, false
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return scannedFile{}, false
	}
	return scannedFile{Path: filepath.ToSlash(rel), Size: info.Size()}, true
}

func (e *Engine) readRepoFile(repos map[string]Repo, key string) (string, bool) {
	rk, rel, ok := strings.Cut(key, "::")
	if !ok {
		return "", false
	}
	repo, found := repos[rk]
	if !found {
		return "", false
	}
	base, err := filepath.Abs(filepath.Join(e.reposDir(), repo.ID))
	if err != nil {
		return "", false
	}
	base = filepath.Clean(base)
	target := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
	if !within(base, target) || target == base {
		return "", false
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxFileBytes {
		return "", false
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return "", false
	}
	return extractPrompt(rel, string(raw)), true
}

func extractPrompt(rel, raw string) string {
	if strings.HasSuffix(strings.ToLower(rel), ".json") {
		var obj any
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			switch v := obj.(type) {
			case string:
				return strings.TrimSpace(v)
			case map[string]any:
				for _, key := range []string{"system_prompt", "systemPrompt", "system", "prompt", "instructions", "content", "text"} {
					if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
			}
		}
	}
	return strings.TrimSpace(raw)
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (e *Engine) httpClient() *http.Client {
	if e.client != nil {
		return e.client
	}
	return &http.Client{Timeout: 60 * time.Second}
}
