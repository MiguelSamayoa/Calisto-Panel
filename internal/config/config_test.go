package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseYAML = `
server: {listen: "0.0.0.0:7171", stale_ms: 10000, dead_ms: 60000, sse_keepalive_ms: 15000}
secrets_file: %q
log: {file: "x.log", level: "info", max_size_mb: 5, max_backups: 5}
sensors:
  url: "http://127.0.0.1:8085/data.json"
  interval_ms: 5000
  timeout_ms: 2000
  items:
    - {id: "cpu.load", label: "CPU", sensor_id: "/intelcpu/0/load/0"}
ci:
  provider: "azure_devops"
  interval_ms: 60000
  timeout_ms: 10000
  error_backoff_max_ms: 300000
  azure_devops:
    organization: "org"
    project: "proj"
    api_version: "7.1"
    pipelines: %s
actions:
  timeout_ms: 60000
  rate_limit_ms: 2000
  docker: {enabled: true, status_running_match: "running"}
  vmware: {enabled: true, vmrun_path: %q, vmx: %q}
  media: {enabled: true, playpause_vk: 0xB3, next_vk: 0xB0}
`

// write builds a config + secrets pair in a temp dir and returns the config path.
func write(t *testing.T, pipelines, secrets string, edit func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	vmrun, vmx := filepath.Join(dir, "vmrun.exe"), filepath.Join(dir, "vm.vmx")
	for _, f := range []string{vmrun, vmx} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sec := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(sec, []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(baseYAML, sec, pipelines, vmrun, vmx)
	if edit != nil {
		body = edit(body)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	noPipelines = "[]"
	onePipeline = `[{id: 42, label: "main"}]`
	tokenOnly   = `api_token: "tok-123"`
	tokenAndPAT = "api_token: tok-123\nazure_devops_pat: pat-456"
)

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, noPipelines, tokenOnly, nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CIEnabled() {
		t.Error("CI enabled without pipelines")
	}
	if cfg.Actions.Media.PlayPauseVK != 0xB3 || cfg.Secrets.APIToken.Reveal() != "tok-123" {
		t.Errorf("unexpected values: %+v", cfg.Actions.Media)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name, pipelines, secrets, wantErr string
		edit                              func(string) string
	}{
		{"stale >= dead", noPipelines, tokenOnly, "stale_ms must be less", func(s string) string { return strings.Replace(s, "stale_ms: 10000", "stale_ms: 60000", 1) }},
		{"bad port", noPipelines, tokenOnly, "port", func(s string) string { return strings.Replace(s, "7171", "99999", 1) }},
		{"bad ci interval", noPipelines, tokenOnly, "ci.interval_ms", func(s string) string { return strings.Replace(s, "interval_ms: 60000", "interval_ms: 5000", 1) }},
		{"empty token", noPipelines, `api_token: ""`, "api_token is required", nil},
		{"pipelines without PAT", onePipeline, tokenOnly, "azure_devops_pat is required", nil},
		{"unknown field", noPipelines, tokenOnly, "field bogus", func(s string) string { return s + "bogus: 1\n" }},
		{"missing vmx", noPipelines, tokenOnly, "vmware.vmx", func(s string) string { return strings.Replace(s, "vm.vmx", "nope.vmx", 1) }},
	}
	for _, c := range cases {
		_, err := Load(write(t, c.pipelines, c.secrets, c.edit))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.wantErr)
		}
	}
}

func TestPipelinesWithPATAreEnabled(t *testing.T) {
	cfg, err := Load(write(t, onePipeline, tokenAndPAT, nil))
	if err != nil || !cfg.CIEnabled() {
		t.Fatalf("Load = %v, %v", cfg, err)
	}
}

func TestSecretsNeverFormatted(t *testing.T) {
	cfg, err := Load(write(t, onePipeline, tokenAndPAT, nil))
	if err != nil {
		t.Fatal(err)
	}
	out := fmt.Sprintf("%v %+v %#v %s", cfg.Secrets, cfg.Secrets, cfg.Secrets, cfg.Secrets.APIToken)
	if strings.Contains(out, "tok-123") || strings.Contains(out, "pat-456") {
		t.Errorf("secret leaked: %s", out)
	}
}
