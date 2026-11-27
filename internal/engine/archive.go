package engine

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxArchiveBytes     = 64 << 20
	maxArchiveEntries   = 100000
	maxArchiveExtracted = 64 << 20
)

// archiveRequest uses the forge's default branch when Branch is empty. Nothing
// is inferred from the browser URL, and credentials never appear in the URL.
func archiveRequest(ctx context.Context, repo Repo) (*http.Request, error) {
	u, err := url.Parse(repo.URL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, UserError("仓库归档地址无效")
	}
	parts := splitPath(u.Path)
	var endpoint, authHeader, authValue string
	switch strings.ToLower(u.Host) {
	case "github.com":
		if len(parts) != 2 {
			return nil, UserError("GitHub 仓库地址必须是 owner/repo")
		}
		endpoint = "https://api.github.com/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/zipball"
		if repo.Branch != "" {
			endpoint += "/" + url.PathEscape(repo.Branch)
		}
		if repo.Token == "" {
			ref := repo.Branch
			if ref == "" {
				ref = "HEAD"
			}
			// Public archives need no REST API quota; private archives use the
			// authenticated API and its short-lived signed redirect.
			endpoint = "https://github.com/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/archive/" + url.PathEscape(ref) + ".zip"
		}
		authHeader, authValue = "Authorization", "Bearer "+repo.Token
	case "gitlab.com":
		if len(parts) < 2 {
			return nil, UserError("GitLab 仓库地址缺少项目路径")
		}
		endpoint = "https://gitlab.com/api/v4/projects/" + url.PathEscape(strings.Join(parts, "/")) + "/repository/archive.zip"
		q := url.Values{"include_lfs_blobs": {"false"}}
		if repo.Branch != "" {
			q.Set("sha", repo.Branch)
		}
		endpoint += "?" + q.Encode()
		authHeader, authValue = "PRIVATE-TOKEN", repo.Token
	case "codeberg.org":
		if len(parts) != 2 {
			return nil, UserError("Codeberg 仓库地址必须是 owner/repo")
		}
		ref := repo.Branch
		if ref == "" {
			ref = "HEAD"
		}
		endpoint = "https://codeberg.org/api/v1/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/archive/" + url.PathEscape(ref) + ".zip"
		authHeader, authValue = "Authorization", "token "+repo.Token
	default:
		return nil, UserError("容器中找不到 git；此仓库主机暂不支持免 Git 归档下载。请使用 GitHub/GitLab.com/Codeberg 仓库或 raw 单文件链接，或在镜像中安装 git")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, UserError("无法创建仓库归档请求")
	}
	req.Header.Set("User-Agent", "prompt-admin")
	req.Header.Set("Accept", "application/zip")
	if repo.Token != "" {
		req.Header.Set(authHeader, authValue)
	}
	return req, nil
}

// pullRepoArchive stages and validates the new snapshot before replacing a cache.
// Existing snapshots (including git clones) survive every download/extract error.
func (e *Engine) pullRepoArchive(ctx context.Context, repo Repo) (string, error) {
	req, err := archiveRequest(ctx, repo)
	if err != nil {
		return "", err
	}
	client := *e.httpClient()
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many archive redirects")
		}
		if next.URL.Scheme != "https" {
			return errors.New("archive redirect must use HTTPS")
		}
		// PRIVATE-TOKEN is not stripped automatically by net/http. Never forward
		// either credential header to a different origin (GitHub signs its zip URLs).
		if next.URL.Host != via[0].URL.Host {
			next.Header.Del("Authorization")
			next.Header.Del("PRIVATE-TOKEN")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", UserError("仓库归档下载失败（检查网络、代理或超时）；原缓存保留")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		switch resp.StatusCode {
		case 401, 403:
			hint = "；请检查仓库 Token 权限或服务限流"
		case 404:
			hint = "；请检查仓库、分支是否存在，以及私有仓库 Token 权限"
		}
		// Do not echo a redirect URL or response body: either can contain a token.
		return "", UserError(fmt.Sprintf("仓库归档下载失败: HTTP %d %s%s；原缓存保留", resp.StatusCode, http.StatusText(resp.StatusCode), hint))
	}
	if resp.ContentLength > maxArchiveBytes {
		return "", UserError("仓库归档超过 64 MiB；原缓存保留")
	}
	if err := os.MkdirAll(e.reposDir(), 0700); err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(e.reposDir(), ".archive-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return "", UserError("仓库归档下载不完整；原缓存保留")
	}
	if size > maxArchiveBytes {
		return "", UserError("仓库归档超过 64 MiB；原缓存保留")
	}
	reader, err := zip.NewReader(archive, size)
	if err != nil {
		return "", UserError("仓库响应不是有效 ZIP 归档；原缓存保留")
	}
	stage, err := os.MkdirTemp(e.reposDir(), ".snapshot-"+repo.ID+"-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := extractPromptArchive(ctx, reader, stage); err != nil {
		return "", err
	}
	if repo.Subpath != "" {
		root := filepath.Join(stage, filepath.FromSlash(repo.Subpath))
		if !within(stage, root) {
			return "", UserError("归档子目录越界；原缓存保留")
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return "", UserError("归档中不存在指定的子目录或其中没有可用提示词；原缓存保留")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := replaceRepoSnapshot(filepath.Join(e.reposDir(), repo.ID), stage); err != nil {
		return "", err
	}
	return "zip:" + hex.EncodeToString(hash.Sum(nil))[:12], nil
}

func extractPromptArchive(ctx context.Context, archive *zip.Reader, stage string) error {
	if len(archive.File) == 0 || len(archive.File) > maxArchiveEntries {
		return UserError("仓库 ZIP 为空或条目过多；原缓存保留")
	}
	prefix := ""
	files := 0
	var total int64
	seen := map[string]bool{}
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name
		// Validate paths before cleaning; reject Windows paths even on Linux.
		if strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
			return UserError("仓库 ZIP 包含不安全路径；原缓存保留")
		}
		parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
		for _, part := range parts {
			if part == ".." || part == "." || part == "" {
				return UserError("仓库 ZIP 包含不安全路径；原缓存保留")
			}
		}
		if prefix == "" {
			prefix = parts[0]
		}
		if parts[0] != prefix || (len(parts) == 1 && !entry.FileInfo().IsDir()) {
			return UserError("仓库 ZIP 缺少统一根目录；原缓存保留")
		}
		if len(parts) < 2 || entry.FileInfo().IsDir() {
			continue
		}
		// Never materialize links, devices, or executables. Only matching prompt
		// files become cache entries; unrelated archive files are never decompressed.
		if !entry.Mode().IsRegular() {
			continue
		}
		rel := strings.Join(parts[1:], "/")
		skip := false
		for _, part := range parts[1 : len(parts)-1] {
			if strings.HasPrefix(part, ".") || skipDir(part) {
				skip = true
				break
			}
		}
		if skip || !allowedExt(strings.ToLower(path.Ext(rel))) || skipName.MatchString(path.Base(rel)) || entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > maxFileBytes {
			continue
		}
		if seen[rel] {
			return UserError("仓库 ZIP 包含重复文件；原缓存保留")
		}
		seen[rel] = true
		files++
		total += int64(entry.UncompressedSize64)
		if files > maxFiles || total > maxArchiveExtracted {
			return UserError("仓库归档的提示词文件过多或超过 64 MiB；原缓存保留")
		}
		target := filepath.Join(stage, filepath.FromSlash(rel))
		if !within(stage, target) {
			return UserError("仓库 ZIP 路径越界；原缓存保留")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		input, err := entry.Open()
		if err != nil {
			return UserError("仓库 ZIP 文件损坏；原缓存保留")
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			input.Close()
			return err
		}
		n, copyErr := io.Copy(output, io.LimitReader(input, maxFileBytes+1))
		input.Close()
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil || n != int64(entry.UncompressedSize64) {
			return UserError("仓库 ZIP 文件损坏或超过限制；原缓存保留")
		}
	}
	if files == 0 {
		return UserError("仓库归档中没有可用的提示词文件（支持 .md/.txt/.json/.yaml/.yml，每个最多 256KB）；原缓存保留")
	}
	return nil
}

func replaceRepoSnapshot(target, stage string) error {
	backup, err := os.MkdirTemp(filepath.Dir(target), ".previous-*")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	hadOld := false
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return err
		}
		hadOld = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, target); err != nil {
		if hadOld {
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				return fmt.Errorf("snapshot replacement failed (%v); cache retained at %s (%v)", err, backup, restoreErr)
			}
		}
		return err
	}
	if hadOld {
		_ = os.RemoveAll(backup)
	}
	return nil
}
