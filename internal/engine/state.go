package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tidwall/gjson"
)

// UserError is a message safe to show in the management page.
type UserError string

func (e UserError) Error() string { return string(e) }

type Repo struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	URL     string `json:"url"`
	Branch  string `json:"branch,omitempty"`
	Subpath string `json:"subpath,omitempty"`
	Token   string `json:"token,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
	AddedAt string `json:"added_at,omitempty"`
}

func (r Repo) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

type Override struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Key  string `json:"key,omitempty"`
}

type Choice struct {
	Key  string   `json:"key"`
	Seen []string `json:"seen"`
	At   string   `json:"at"`
}

type State struct {
	Repos         []Repo              `json:"repos"`
	Overrides     map[string]Override `json:"overrides"`
	Local         map[string]string   `json:"local"`
	Choices       map[string]Choice   `json:"choices"`
	DefaultPrompt string              `json:"default_prompt"`
	LastOutput    map[string]string   `json:"-"`
	HasLast       bool                `json:"-"`
}

type diskState struct {
	Repos         []Repo              `json:"repos"`
	Overrides     map[string]Override `json:"overrides"`
	Local         map[string]string   `json:"local"`
	Choices       map[string]Choice   `json:"choices"`
	DefaultPrompt string              `json:"default_prompt"`
	LastOutput    *map[string]string  `json:"last_output,omitempty"`
}

func emptyState() State {
	return State{
		Repos:     []Repo{},
		Overrides: map[string]Override{},
		Local:     map[string]string{},
		Choices:   map[string]Choice{},
	}
}

func (s *State) normalize() {
	if s.Repos == nil {
		s.Repos = []Repo{}
	}
	if s.Overrides == nil {
		s.Overrides = map[string]Override{}
	}
	if s.Local == nil {
		s.Local = map[string]string{}
	}
	if s.Choices == nil {
		s.Choices = map[string]Choice{}
	}
}

func loadState(path string) State {
	raw, err := os.ReadFile(path)
	if err != nil {
		return emptyState()
	}
	var disk diskState
	if err := json.Unmarshal(raw, &disk); err != nil {
		return emptyState()
	}
	st := State{
		Repos:         disk.Repos,
		Overrides:     disk.Overrides,
		Local:         disk.Local,
		Choices:       disk.Choices,
		DefaultPrompt: disk.DefaultPrompt,
		HasLast:       disk.LastOutput != nil,
	}
	if disk.LastOutput != nil {
		st.LastOutput = *disk.LastOutput
	}
	st.normalize()
	return st
}

func saveState(path string, st State) error {
	st.normalize()
	disk := diskState{
		Repos:         st.Repos,
		Overrides:     st.Overrides,
		Local:         st.Local,
		Choices:       st.Choices,
		DefaultPrompt: st.DefaultPrompt,
	}
	if st.HasLast {
		copied := cloneMap(st.LastOutput)
		disk.LastOutput = &copied
	}
	return writeJSON(path, disk, 0o600)
}

func writeJSON(path string, value any, mode os.FileMode) error {
	raw, err := marshalPretty(value)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw, mode)
}

func marshalPretty(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeAtomic(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

type promptFile struct {
	Prompts map[string]string
	Order   []string
}

func readPromptFile(path string) promptFile {
	raw, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return promptFile{Prompts: map[string]string{}}
	}
	parsed := gjson.ParseBytes(raw)
	if !parsed.IsObject() {
		return promptFile{Prompts: map[string]string{}}
	}
	out := promptFile{Prompts: map[string]string{}}
	parsed.ForEach(func(key, value gjson.Result) bool {
		if value.Type != gjson.String {
			return true
		}
		out.Order = append(out.Order, key.String())
		out.Prompts[key.String()] = value.String()
		return true
	})
	return out
}

func writePromptFile(path string, order []string, prompts map[string]string) error {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, key := range order {
		kb, err := marshalString(key)
		if err != nil {
			return err
		}
		vb, err := marshalString(prompts[key])
		if err != nil {
			return err
		}
		buf.WriteString("  ")
		buf.Write(kb)
		buf.WriteString(": ")
		buf.Write(vb)
		if i+1 < len(order) {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	// Validate before replacing the live file.
	if !gjson.ValidBytes(buf.Bytes()) {
		return errors.New("refusing to write invalid prompts.json")
	}
	return writeAtomic(path, buf.Bytes(), 0o600)
}

func marshalString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func backupPrompts(backupDir, promptsPath string, now time.Time) error {
	if _, err := os.Stat(promptsPath); err != nil {
		return nil
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("prompts.%s.json", now.Format("20060102-150405.000"))
	raw, err := os.ReadFile(promptsPath)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(backupDir, name), raw, 0o600); err != nil {
		return err
	}
	entries, err := filepath.Glob(filepath.Join(backupDir, "prompts.*.json"))
	if err != nil {
		return nil
	}
	if len(entries) <= 60 {
		return nil
	}
	// Glob is sorted by name, which is the timestamp.
	for _, old := range entries[:len(entries)-60] {
		_ = os.Remove(old)
	}
	return nil
}
