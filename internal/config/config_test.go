package config

import "testing"

func TestParseDefaultsAndRejectsUnknown(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.CPAURL != DefaultCPAURL || cfg.DataDir != "prompt-admin-data" || cfg.WatchInterval.String() != "15s" || cfg.SyncInterval.String() != "1h0m0s" {
		t.Fatalf("defaults = %+v", cfg)
	}
	if _, err := Parse([]byte("enabled: true\nnot_a_field: 1\n")); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestParseAllowsStoreAndDurations(t *testing.T) {
	raw := []byte(`
enabled: true
priority: 100
data_dir: /var/lib/prompt-admin
cpa_url: http://127.0.0.1:8317/
cpa_config: config.yaml
watch_interval: 15
sync_interval: 0s
allow_file_urls: false
store:
  source: example
`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPAURL != "http://127.0.0.1:8317" || cfg.WatchInterval.Seconds() != 15 || cfg.SyncInterval != 0 || cfg.DataDir != "/var/lib/prompt-admin" {
		t.Fatalf("parsed = %+v", cfg)
	}
}

func TestRejectsBadURL(t *testing.T) {
	if _, err := Parse([]byte("cpa_url: ftp://example\n")); err == nil {
		t.Fatal("ftp url accepted")
	}
}
