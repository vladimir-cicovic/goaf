package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	LastInventory string `yaml:"last_inventory,omitempty"`
	Parallel      int    `yaml:"parallel"`
	MonSplit      int    `yaml:"mon_split"`
	DryRun        bool   `yaml:"dry_run,omitempty"`
	Become        bool   `yaml:"become,omitempty"`
	Report        bool   `yaml:"report,omitempty"`
	ReportPath    string `yaml:"report_path,omitempty"`
}

// Default returns a Config with sensible out-of-the-box values.
func Default() Config {
	return Config{
		Parallel:   10,
		MonSplit:   50,
		ReportPath: "/tmp/goaf-report.json",
	}
}

func path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "goaf-tui", "config.yml"), nil
}

// Load reads the config file. Returns defaults silently if file doesn't exist.
func Load() Config {
	cfg := Default()
	p, err := path()
	if err != nil {
		return cfg
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return cfg
	}
	_ = yaml.Unmarshal(data, &cfg)
	if cfg.Parallel <= 0 {
		cfg.Parallel = 10
	}
	if cfg.MonSplit <= 0 {
		cfg.MonSplit = 50
	}
	return cfg
}

// Save writes config atomically. Errors are silently ignored by callers.
func Save(cfg Config) error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
}
