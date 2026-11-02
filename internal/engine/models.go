package engine

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type modelSnap struct {
	IDs      []string
	Upstream map[string]string
	Source   string
}

type namesDoc struct {
	Aliases  map[string]string
	Provider map[string]string
	Pointer  map[string]bool
}

type signature struct {
	Hash        string `json:"hash"`
	Source      string `json:"source"`
	ConfigMtime int64  `json:"config_mtime"`
	NamesMtime  int64  `json:"names_mtime"`
}

func (e *Engine) fetchModels() modelSnap {
	if e.fetch != nil {
		return e.fetch()
	}
	key, upstream := readCPAConfig(e.cfg.CPAConfig)
	if strings.TrimSpace(e.cfg.APIKey) != "" {
		key = strings.TrimSpace(e.cfg.APIKey)
	}
	if key != "" && e.cfg.CPAURL != "" {
		ids, err := e.fetchModelIDs(key)
		if err != nil {
			e.logf("WARN /v1/models failed (%s); using config aliases or the static model list", err.Error())
		} else if len(ids) > 0 {
			return modelSnap{IDs: ids, Upstream: upstream, Source: "CPA /v1/models"}
		}
	}
	if len(upstream) > 0 {
		ids := make([]string, 0, len(upstream))
		for alias := range upstream {
			ids = append(ids, alias)
		}
		sort.Strings(ids)
		return modelSnap{IDs: ids, Upstream: upstream, Source: "config.yaml"}
	}
	if len(e.cfg.Models) > 0 {
		ids := append([]string(nil), e.cfg.Models...)
		sort.Strings(ids)
		return modelSnap{IDs: ids, Upstream: map[string]string{}, Source: "plugin config"}
	}
	return modelSnap{Upstream: map[string]string{}, Source: "none"}
}

func (e *Engine) fetchModelIDs(key string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(e.cfg.CPAURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	client := e.httpClient()
	client = &http.Client{Timeout: 20 * time.Second, Transport: client.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, UserError("HTTP " + resp.Status)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

var (
	accessBlockRE = regexp.MustCompile(`(?m)^access:\s*\n((?:[ \t]+.*\n|\s*\n)+)`)
	apiKeyRE      = regexp.MustCompile(`api-keys:\s*\n(?:\s*#.*\n)*\s*-\s*["']?([^"'\s#]+)`)
	modelNameRE   = regexp.MustCompile(`^\s+-\s+name:\s*["']?([^"'#]+?)["']?\s*$`)
	aliasRE       = regexp.MustCompile(`^\s+alias:\s*["']?([^"'#]+?)["']?\s*$`)
)

func readCPAConfig(path string) (string, map[string]string) {
	upstream := map[string]string{}
	if strings.TrimSpace(path) == "" {
		return "", upstream
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", upstream
	}
	text := string(raw)
	key := ""
	if m := accessBlockRE.FindStringSubmatch(text); len(m) == 2 {
		if k := apiKeyRE.FindStringSubmatch(m[1]); len(k) == 2 {
			key = k[1]
		}
	}
	pending := ""
	for _, line := range strings.Split(text, "\n") {
		if mn := modelNameRE.FindStringSubmatch(line); len(mn) == 2 {
			pending = strings.TrimSpace(mn[1])
			continue
		}
		if ma := aliasRE.FindStringSubmatch(line); len(ma) == 2 && pending != "" {
			upstream[strings.TrimSpace(ma[1])] = pending
			pending = ""
		}
	}
	return key, upstream
}

func loadNames(path string) namesDoc {
	doc := namesDoc{Aliases: map[string]string{}, Provider: map[string]string{}, Pointer: map[string]bool{}}
	if strings.TrimSpace(path) == "" {
		return doc
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc
	}
	var payload struct {
		Aliases map[string]string `json:"aliases"`
		Models  []struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"models"`
		Pointers []struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"pointers"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return doc
	}
	for old, next := range payload.Aliases {
		if strings.TrimSpace(next) == "" {
			continue
		}
		doc.Aliases[old] = next
	}
	for _, model := range payload.Models {
		if model.Name != "" {
			doc.Provider[model.Name] = model.Provider
		}
	}
	for _, pointer := range payload.Pointers {
		if pointer.Name != "" {
			doc.Pointer[pointer.Name] = true
			if _, ok := doc.Provider[pointer.Name]; !ok {
				doc.Provider[pointer.Name] = pointer.Provider
			}
		}
	}
	return doc
}

func (e *Engine) signature(snap modelSnap) signature {
	sum := sha1.Sum([]byte(strings.Join(snap.IDs, "\n")))
	return signature{
		Hash:        hex.EncodeToString(sum[:])[:16],
		Source:      snap.Source,
		ConfigMtime: fileMtime(e.cfg.CPAConfig),
		NamesMtime:  fileMtime(e.cfg.NamesFile),
	}
}

func fileMtime(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}
