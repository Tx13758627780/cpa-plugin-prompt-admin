package engine

import (
	"crypto/sha1"
	"encoding/hex"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

func parseSource(rawURL, branch, subpath string, allowFile bool) (Repo, error) {
	rawURL = strings.TrimSpace(rawURL)
	branch = strings.TrimSpace(branch)
	subpath = strings.Trim(strings.TrimSpace(subpath), "/")
	if regexp.MustCompile(`^[\w.-]+/[\w.-]+$`).MatchString(rawURL) {
		rawURL = "https://github.com/" + rawURL
	}
	if allowFile && strings.HasPrefix(rawURL, "file:///") {
		return finishSource(Repo{Kind: "git", URL: strings.TrimRight(rawURL, "/"), Branch: branch, Subpath: subpath})
	}
	if !regexp.MustCompile(`^https?://\S+$`).MatchString(rawURL) {
		return Repo{}, UserError("只支持 http(s) 链接，或 owner/repo 简写")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return Repo{}, UserError("只支持 http(s) 链接，或 owner/repo 简写")
	}
	host := strings.ToLower(u.Host)
	parts := splitPath(u.Path)
	ext := strings.ToLower(path.Ext(u.Path))
	kind := "git"
	switch {
	case host == "github.com" && len(parts) >= 2:
		owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
		if len(parts) >= 5 && parts[2] == "blob" {
			raw := "https://raw.githubusercontent.com/" + owner + "/" + repo + "/" + parts[3] + "/" + strings.Join(parts[4:], "/")
			return finishSource(Repo{Kind: "raw", URL: raw})
		}
		if len(parts) >= 4 && parts[2] == "tree" {
			if branch == "" {
				branch = parts[3]
			}
			if subpath == "" {
				subpath = strings.Join(parts[4:], "/")
			}
		}
		rawURL = "https://github.com/" + owner + "/" + repo
	case host == "raw.githubusercontent.com" || host == "gist.githubusercontent.com" || (allowedExt(ext) && (strings.Contains(u.Path, "/raw/") || strings.Contains(u.Path, "/-/raw/"))):
		kind = "raw"
		branch, subpath = "", ""
	case strings.Contains(u.Path, "/-/tree/"):
		base, rest, _ := strings.Cut(u.Path, "/-/tree/")
		rp := splitPath(rest)
		if branch == "" && len(rp) > 0 {
			branch = rp[0]
		}
		if subpath == "" {
			subpath = strings.Join(rp[1:], "/")
		}
		rawURL = u.Scheme + "://" + u.Host + strings.TrimRight(base, "/")
	case strings.Contains(u.Path, "/src/branch/"):
		base, rest, _ := strings.Cut(u.Path, "/src/branch/")
		rp := splitPath(rest)
		if branch == "" && len(rp) > 0 {
			branch = rp[0]
		}
		if subpath == "" {
			subpath = strings.Join(rp[1:], "/")
		}
		rawURL = u.Scheme + "://" + u.Host + strings.TrimRight(base, "/")
	default:
		rawURL = strings.TrimRight(rawURL, "/")
	}
	if kind == "git" {
		rawURL = strings.TrimSuffix(strings.TrimRight(rawURL, "/"), ".git")
	}
	return finishSource(Repo{Kind: kind, URL: rawURL, Branch: branch, Subpath: subpath})
}

func finishSource(repo Repo) (Repo, error) {
	if strings.Contains(repo.Subpath, "..") {
		for _, part := range strings.Split(repo.Subpath, "/") {
			if part == ".." {
				return Repo{}, UserError("子目录不能包含 ..")
			}
		}
	}
	if repo.Branch != "" && !regexp.MustCompile(`^[\w./-]+$`).MatchString(repo.Branch) {
		return Repo{}, UserError("分支名不合法")
	}
	if repo.URL == "" || strings.ContainsAny(repo.URL, " \t\r\n") {
		return Repo{}, UserError("只支持 http(s) 链接，或 owner/repo 简写")
	}
	repo.ID = repoID(repo)
	return repo, nil
}

func repoID(repo Repo) string {
	sum := sha1.Sum([]byte(repo.Kind + "|" + repo.URL + "|" + repo.Branch))
	return hex.EncodeToString(sum[:])[:12]
}

func repoKey(repo Repo) string {
	branch := repo.Branch
	if branch == "" {
		branch = "HEAD"
	}
	return repo.URL + "@" + branch
}

func splitPath(p string) []string {
	var out []string
	for _, part := range strings.Split(p, "/") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func allowedExt(ext string) bool {
	switch ext {
	case ".md", ".txt", ".json", ".yaml", ".yml":
		return true
	default:
		return false
	}
}

func fileLabel(repo Repo, rel string) string {
	short := regexp.MustCompile(`^https?://(www\.)?`).ReplaceAllString(repo.URL, "")
	br := ""
	if repo.Branch != "" {
		br = "@" + repo.Branch
	}
	if rel == "" {
		return short + br
	}
	return short + br + " · " + rel
}

// within reports whether target is base or a file inside base.
func within(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
