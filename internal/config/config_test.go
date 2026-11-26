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
	t.Setenv("ProgramData", dir)
	body := fmt.Sprintf(baseYAML, sec, pipelines, vmrun, vmx)
	logPath := filepath.ToSlash(filepath.Join(dir, "CalistoPanel", "x.log"))
	body = strings.Replace(body, `"x.log"`, `"`+logPath+`"`, 1)
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
	tokenOnly   = `api_token: "tok-123-0123456789abcdef0123456789abcdef"`
	tokenAndPAT = "api_token: tok-123-0123456789abcdef0123456789abcdef\nazure_devops_pat: pat-456"
)

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, noPipelines, tokenOnly, nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CIEnabled() {
		t.Error("CI enabled without pipelines")
	}
	if cfg.Actions.Media.PlayPauseVK != 0xB3 || cfg.Secrets.APIToken.Reveal() != "tok-123-0123456789abcdef0123456789abcdef" {
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
		{"short token", noPipelines, `api_token: "short-token"`, "at least 32", nil},
		{"log outside ProgramData", noPipelines, tokenOnly, "log.file must be inside", func(s string) string { return strings.Replace(s, "CalistoPanel/x.log", "elsewhere/x.log", 1) }},
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
	if strings.Contains(out, "tok-123-0123456789abcdef0123456789abcdef") || strings.Contains(out, "pat-456") {
		t.Errorf("secret leaked: %s", out)
	}
}

// A config written before the processes and containers sections existed must still load.
func TestOptionalSectionsTakeDefaults(t *testing.T) {
	cfg, err := Load(write(t, noPipelines, tokenOnly, nil))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Processes; p.IntervalMs != 5000 || p.TopN != 5 || len(p.IncludeUnreadable) != 2 {
		t.Errorf("processes defaults = %+v", p)
	}
	if c := cfg.Containers; c.IntervalMs != 5000 || c.TimeoutMs != 8000 {
		t.Errorf("containers defaults = %+v", c)
	}
}

func TestOptionalSectionsOverride(t *testing.T) {
	cfg, err := Load(write(t, noPipelines, tokenOnly, func(s string) string {
		return s + "processes: {interval_ms: 2000, top_n: 3, exclude: [panel.exe], include_unreadable: []}\ncontainers: {interval_ms: 10000, timeout_ms: 4000}\n"
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Processes; p.IntervalMs != 2000 || p.TopN != 3 || len(p.Exclude) != 1 || len(p.IncludeUnreadable) != 0 {
		t.Errorf("processes = %+v", p)
	}
	if cfg.Containers.IntervalMs != 10000 || cfg.Containers.TimeoutMs != 4000 {
		t.Errorf("containers = %+v", cfg.Containers)
	}
}

func TestMediaAndVMDefaultsAndDeprecatedRateLimit(t *testing.T) {
	// baseYAML still carries rate_limit_ms: 2000, which is accepted and ignored.
	cfg, err := Load(write(t, noPipelines, tokenOnly, nil))
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Actions.Media; m.PrevVK != 0xB1 || m.IntervalMs != 2000 {
		t.Errorf("media defaults = %+v", m)
	}
	if cfg.VM.IntervalMs != 5000 || cfg.VM.TimeoutMs != 5000 {
		t.Errorf("vm defaults = %+v", cfg.VM)
	}
	cfg, err = Load(write(t, noPipelines, tokenOnly, func(s string) string {
		return strings.Replace(s, "  rate_limit_ms: 2000\n", "", 1)
	}))
	if err != nil {
		t.Errorf("a config without rate_limit_ms must load: %v", err)
	}
}
