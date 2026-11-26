package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      Server  `yaml:"server"`
	SecretsFile string  `yaml:"secrets_file"`
	Log         Log     `yaml:"log"`
	Sensors     Sensors `yaml:"sensors"`
	CI          CI      `yaml:"ci"`
	Actions     Actions `yaml:"actions"`
	// Processes and Containers are optional: absent sections take the defaults below.
	Processes  Processes  `yaml:"processes"`
	Containers Containers `yaml:"containers"`
	VM         VM         `yaml:"vm"`
	Secrets    Secrets    `yaml:"-"`
}

// Processes configures the "apps that consume the most" poller. A process is an app
// when its image lives outside SystemRoot; Exclude drops noisy ones by file name and
// IncludeUnreadable keeps named processes whose path cannot be read (vmmem for WSL2).
type Processes struct {
	IntervalMs        int      `yaml:"interval_ms"`
	TopN              int      `yaml:"top_n"`
	Exclude           []string `yaml:"exclude"`
	IncludeUnreadable []string `yaml:"include_unreadable"`
}

// Containers configures the Docker container poller. It runs only when
// actions.docker.enabled is true.
type Containers struct {
	IntervalMs int `yaml:"interval_ms"`
	TimeoutMs  int `yaml:"timeout_ms"`
}

// VM configures the VMware poller (state and host-side CPU/RAM of the configured VM).
// It runs only when actions.vmware.enabled is true.
type VM struct {
	IntervalMs int `yaml:"interval_ms"`
	TimeoutMs  int `yaml:"timeout_ms"`
}

func (c *Config) applyDefaults() {
	if c.VM.IntervalMs == 0 {
		c.VM.IntervalMs = 5000
	}
	if c.VM.TimeoutMs == 0 {
		c.VM.TimeoutMs = 5000
	}
	if c.Actions.Media.PrevVK == 0 {
		c.Actions.Media.PrevVK = 0xB1 // VK_MEDIA_PREV_TRACK
	}
	if c.Actions.Media.IntervalMs == 0 {
		c.Actions.Media.IntervalMs = 2000
	}
	if c.Processes.IntervalMs == 0 {
		c.Processes.IntervalMs = 5000
	}
	if c.Processes.TopN == 0 {
		c.Processes.TopN = 5
	}
	if c.Processes.IncludeUnreadable == nil {
		c.Processes.IncludeUnreadable = []string{"vmmem", "vmmemWSL"}
	}
	if c.Containers.IntervalMs == 0 {
		c.Containers.IntervalMs = 5000
	}
	if c.Containers.TimeoutMs == 0 {
		c.Containers.TimeoutMs = 8000
	}
}

type Server struct {
	Listen         string `yaml:"listen"`
	StaleMs        int    `yaml:"stale_ms"`
	DeadMs         int    `yaml:"dead_ms"`
	SSEKeepaliveMs int    `yaml:"sse_keepalive_ms"`
}

type Log struct {
	File       string `yaml:"file"`
	Level      string `yaml:"level"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxBackups int    `yaml:"max_backups"`
}

type Sensors struct {
	URL        string       `yaml:"url"`
	IntervalMs int          `yaml:"interval_ms"`
	TimeoutMs  int          `yaml:"timeout_ms"`
	Items      []SensorItem `yaml:"items"`
}

type SensorItem struct {
	ID       string `yaml:"id"`
	Label    string `yaml:"label"`
	SensorID string `yaml:"sensor_id"`
}

type CI struct {
	Provider          string      `yaml:"provider"`
	IntervalMs        int         `yaml:"interval_ms"`
	TimeoutMs         int         `yaml:"timeout_ms"`
	ErrorBackoffMaxMs int         `yaml:"error_backoff_max_ms"`
	AzureDevOps       AzureDevOps `yaml:"azure_devops"`
}

type AzureDevOps struct {
	Organization string     `yaml:"organization"`
	Project      string     `yaml:"project"`
	APIVersion   string     `yaml:"api_version"`
	Pipelines    []Pipeline `yaml:"pipelines"`
}

type Pipeline struct {
	ID    int    `yaml:"id"`
	Label string `yaml:"label"`
}

type Actions struct {
	TimeoutMs int `yaml:"timeout_ms"`
	// RateLimitMs is deprecated and ignored: there is no global rate limit any more
	// (see ARCHITECTURE §4). It stays so an existing config.yaml still loads.
	RateLimitMs     int      `yaml:"rate_limit_ms"`
	ConfirmRequired []string `yaml:"confirm_required"`
	Docker          Docker   `yaml:"docker"`
	VMware          VMware   `yaml:"vmware"`
	Media           Media    `yaml:"media"`
}

type Docker struct {
	Enabled            bool   `yaml:"enabled"`
	StatusRunningMatch string `yaml:"status_running_match"`
}

type VMware struct {
	Enabled   bool   `yaml:"enabled"`
	VmrunPath string `yaml:"vmrun_path"`
	VMX       string `yaml:"vmx"`
}

type Media struct {
	Enabled     bool  `yaml:"enabled"`
	PlayPauseVK uint8 `yaml:"playpause_vk"`
	NextVK      uint8 `yaml:"next_vk"`
	PrevVK      uint8 `yaml:"prev_vk"`     // optional, default VK_MEDIA_PREV_TRACK
	IntervalMs  int   `yaml:"interval_ms"` // now-playing poll, default 2000
}

// Secrets never leave the server. Both fields redact themselves when formatted or
// logged, so a stray %v or slog attribute cannot leak them.
type Secrets struct {
	APIToken       Secret `yaml:"api_token"`
	AzureDevOpsPAT Secret `yaml:"azure_devops_pat"`
}

type Secret string

func (Secret) String() string       { return "[redacted]" }
func (Secret) GoString() string     { return `"[redacted]"` }
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }
func (s Secret) Reveal() string     { return string(s) }

// CIEnabled reports whether any pipeline is configured. With none, CI is off.
func (c *Config) CIEnabled() bool { return len(c.CI.AzureDevOps.Pipelines) > 0 }

// Load reads and validates the config file and the secrets file it points to.
// Any failure is an operator error and must abort startup.
func Load(path string) (*Config, error) {
	var cfg Config
	if err := decodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if cfg.SecretsFile == "" {
		return nil, errors.New("secrets_file is required")
	}
	if err := decodeFile(cfg.SecretsFile, &cfg.Secrets); err != nil {
		return nil, fmt.Errorf("loading secrets: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func decodeFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if _, port, err := net.SplitHostPort(c.Server.Listen); err != nil {
		bad("server.listen: %v", err)
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		bad("server.listen: port %q out of range", port)
	}
	for name, v := range map[string]int{
		"server.stale_ms":           c.Server.StaleMs,
		"server.sse_keepalive_ms":   c.Server.SSEKeepaliveMs,
		"log.max_size_mb":           c.Log.MaxSizeMB,
		"log.max_backups":           c.Log.MaxBackups,
		"sensors.interval_ms":       c.Sensors.IntervalMs,
		"sensors.timeout_ms":        c.Sensors.TimeoutMs,
		"ci.interval_ms":            c.CI.IntervalMs,
		"ci.timeout_ms":             c.CI.TimeoutMs,
		"ci.error_backoff_max_ms":   c.CI.ErrorBackoffMaxMs,
		"actions.timeout_ms":        c.Actions.TimeoutMs,
		"vm.interval_ms":            c.VM.IntervalMs,
		"vm.timeout_ms":             c.VM.TimeoutMs,
		"actions.media.interval_ms": c.Actions.Media.IntervalMs,
		"processes.interval_ms":     c.Processes.IntervalMs,
		"processes.top_n":           c.Processes.TopN,
		"containers.interval_ms":    c.Containers.IntervalMs,
		"containers.timeout_ms":     c.Containers.TimeoutMs,
	} {
		if v <= 0 {
			bad("%s must be positive", name)
		}
	}
	if c.Server.StaleMs >= c.Server.DeadMs {
		bad("server.stale_ms must be less than server.dead_ms")
	}
	if c.Log.File == "" {
		bad("log.file is required")
	} else if pd := os.Getenv("ProgramData"); pd != "" && !underDir(c.Log.File, filepath.Join(pd, "CalistoPanel")) {
		bad(`log.file must be inside %%ProgramData%%\CalistoPanel\ (got %q)`, c.Log.File)
	}
	if err := new(slog.Level).UnmarshalText([]byte(c.Log.Level)); err != nil {
		bad("log.level %q: want debug|info|warn|error", c.Log.Level)
	}

	if c.Sensors.URL == "" {
		bad("sensors.url is required")
	}
	seen := map[string]bool{}
	for i, it := range c.Sensors.Items {
		switch {
		case it.ID == "" || it.Label == "" || it.SensorID == "":
			bad("sensors.items[%d]: id, label and sensor_id are required", i)
		case seen[it.ID]:
			bad("sensors.items[%d]: duplicate id %q", i, it.ID)
		}
		seen[it.ID] = true
	}

	if c.CI.Provider != "azure_devops" {
		bad("ci.provider %q: only azure_devops is supported", c.CI.Provider)
	}
	if c.CI.IntervalMs != 60000 && c.CI.IntervalMs != 150000 {
		bad("ci.interval_ms must be 60000 or 150000")
	}
	if c.CIEnabled() {
		ado := c.CI.AzureDevOps
		if ado.Organization == "" || ado.Project == "" || ado.APIVersion == "" {
			bad("ci.azure_devops: organization, project and api_version are required with pipelines")
		}
		if len(ado.Pipelines) > 5 {
			bad("ci.azure_devops.pipelines: at most 5")
		}
		for i, p := range ado.Pipelines {
			if p.ID <= 0 || p.Label == "" {
				bad("ci.azure_devops.pipelines[%d]: id and label are required", i)
			}
		}
		if c.Secrets.AzureDevOpsPAT == "" {
			bad("secrets: azure_devops_pat is required when pipelines are configured")
		}
	}

	a := c.Actions
	if !a.Docker.Enabled && !a.VMware.Enabled && !a.Media.Enabled {
		bad("actions: at least one of docker, vmware, media must be enabled")
	}
	if a.Docker.Enabled && a.Docker.StatusRunningMatch == "" {
		bad("actions.docker.status_running_match is required")
	}
	if a.VMware.Enabled {
		for name, p := range map[string]string{"vmrun_path": a.VMware.VmrunPath, "vmx": a.VMware.VMX} {
			if _, err := os.Stat(p); err != nil {
				bad("actions.vmware.%s: %v", name, err)
			}
		}
	}
	if a.Media.Enabled && (a.Media.PlayPauseVK == 0 || a.Media.NextVK == 0) {
		bad("actions.media: playpause_vk and next_vk are required")
	}

	switch n := len(c.Secrets.APIToken); {
	case n == 0:
		bad("secrets: api_token is required")
	case n < minTokenLen:
		bad("secrets: api_token must be at least %d characters (it is the only thing between the LAN and the action buttons)", minTokenLen)
	}
	return errors.Join(errs...)
}

// minTokenLen is the shortest api_token accepted: 32 random bytes in base64 is 44.
const minTokenLen = 32

// underDir reports whether path is strictly inside dir. A relative path is never inside.
func underDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
