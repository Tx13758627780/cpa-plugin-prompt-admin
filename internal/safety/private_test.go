package safety

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRepoHasNoPrivateDeployDetails scans the source tree for things that should
// never ship in a public plugin: routable IPv4 literals, real-looking API keys,
// personal e-mail addresses, and absolute home or deploy paths.
func TestRepoHasNoPrivateDeployDetails(t *testing.T) {
	root := moduleRoot(t)
	checks := []struct {
		name string
		re   *regexp.Regexp
		ok   func(string) bool
	}{
		{"ipv4", regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`), func(s string) bool {
			return s == "127.0.0.1" || s == "0.0.0.0"
		}},
		{"api key", regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gsk_[A-Za-z0-9]{16,}|AIza[0-9A-Za-z_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), nil},
		{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), func(s string) bool {
			return strings.HasSuffix(s, "@example.com") || strings.HasSuffix(s, "@users.noreply.github.com")
		}},
		{"home path", regexp.MustCompile(`/(?:home|Users)/[a-z][A-Za-z0-9_-]+/`), nil},
	}
	skipDir := map[string]bool{".git": true, "dist": true, "vendor": true, ".cache": true}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if skipDir[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if name == "go.sum" || strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tgz") {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 1<<20 {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, c := range checks {
			for _, hit := range c.re.FindAllString(string(body), -1) {
				if c.ok != nil && c.ok(hit) {
					continue
				}
				t.Errorf("%s: %s %q", rel, c.name, hit)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
