// Package config parses the prompt-admin plugin block.
// Unknown keys fail registration so a typo cannot silently disable injection.
package config

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	PluginID      = "prompt-admin"
	DefaultCPAURL = "http://127.0.0.1:8317"
	minWatch      = time.Second
	maxWatch      = 24 * time.Hour
	minSync       = time.Minute
	maxSync       = 7 * 24 * time.Hour
)

// Config is the validated plugin configuration.
type Config struct {
	Enabled       bool
	Priority      int
	DataDir       string
	CPAURL        string
	CPAConfig     string
	APIKey        string
	NamesFile     string
	Models        []string
	WatchInterval time.Duration
	SyncInterval  time.Duration
	AllowFileURLs bool
}

type fileConfig struct {
	Enabled       *bool     `yaml:"enabled"`
	Priority      *int      `yaml:"priority"`
	DataDir       *string   `yaml:"data_dir"`
	CPAURL        *string   `yaml:"cpa_url"`
	CPAConfig     *string   `yaml:"cpa_config"`
	APIKey        *string   `yaml:"api_key"`
	NamesFile     *string   `yaml:"names_file"`
	Models        []string  `yaml:"models"`
	WatchInterval *Duration `yaml:"watch_interval"`
	SyncInterval  *Duration `yaml:"sync_interval"`
	AllowFileURLs *bool     `yaml:"allow_file_urls"`
	// Store is written by the CPA plugin store and is ignored.
	Store yaml.Node `yaml:"store"`
}

// Duration accepts a Go duration string or an integer number of seconds.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string or a number of seconds")
	}
	if value.Tag == "!!int" || value.Tag == "!!float" {
		var n float64
		if err := value.Decode(&n); err != nil {
			return err
		}
		if n < 0 {
			return fmt.Errorf("duration cannot be negative")
		}
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	var text string
	if err := value.Decode(&text); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" || text == "0" || text == "0s" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q", text)
	}
	*d = Duration(parsed)
	return nil
}

// Parse decodes the plugin YAML passed by CPA on register and reconfigure.
func Parse(raw []byte) (Config, error) {
	cfg := Config{
		Enabled:       true,
		CPAURL:        DefaultCPAURL,
		DataDir:       "prompt-admin-data",
		WatchInterval: 15 * time.Second,
		SyncInterval:  time.Hour,
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	var file fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return Config{}, fmt.Errorf("decode %s config: %w", PluginID, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("decode %s config: multiple YAML documents are not supported", PluginID)
		}
		return Config{}, fmt.Errorf("decode %s config: %w", PluginID, err)
	}
	if file.Enabled != nil {
		cfg.Enabled = *file.Enabled
	}
	if file.Priority != nil {
		cfg.Priority = *file.Priority
	}
	if file.DataDir != nil {
		cfg.DataDir = strings.TrimSpace(*file.DataDir)
	}
	if file.CPAURL != nil && strings.TrimSpace(*file.CPAURL) != "" {
		cfg.CPAURL = strings.TrimRight(strings.TrimSpace(*file.CPAURL), "/")
	}
	if file.CPAConfig != nil {
		cfg.CPAConfig = strings.TrimSpace(*file.CPAConfig)
	}
	if file.APIKey != nil {
		cfg.APIKey = strings.TrimSpace(*file.APIKey)
	}
	if file.NamesFile != nil {
		cfg.NamesFile = strings.TrimSpace(*file.NamesFile)
	}
	if file.WatchInterval != nil {
		cfg.WatchInterval = time.Duration(*file.WatchInterval)
	}
	if file.SyncInterval != nil {
		cfg.SyncInterval = time.Duration(*file.SyncInterval)
	}
	if file.AllowFileURLs != nil {
		cfg.AllowFileURLs = *file.AllowFileURLs
	}
	for _, model := range file.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			return Config{}, fmt.Errorf("models entries must be non-empty")
		}
		cfg.Models = append(cfg.Models, model)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg Config) validate() error {
	if cfg.DataDir == "" || strings.ContainsRune(cfg.DataDir, 0) {
		return fmt.Errorf("data_dir is required")
	}
	if cfg.DataDir == "/" || cfg.DataDir == "." {
		return fmt.Errorf("data_dir %q is too broad", cfg.DataDir)
	}
	u, err := url.Parse(cfg.CPAURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("cpa_url must be an http(s) URL")
	}
	if strings.ContainsRune(cfg.CPAConfig, 0) || strings.ContainsRune(cfg.NamesFile, 0) || strings.ContainsRune(cfg.APIKey, 0) {
		return fmt.Errorf("config paths and api_key must not contain NUL")
	}
	if cfg.WatchInterval < minWatch || cfg.WatchInterval > maxWatch {
		return fmt.Errorf("watch_interval must be between %s and %s", minWatch, maxWatch)
	}
	if cfg.SyncInterval != 0 && (cfg.SyncInterval < minSync || cfg.SyncInterval > maxSync) {
		return fmt.Errorf("sync_interval must be 0 (disabled) or between %s and %s", minSync, maxSync)
	}
	return nil
}
