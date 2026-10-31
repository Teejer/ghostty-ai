// Package config loads ghostty-ai configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Provider is an OpenAI-compatible chat completions endpoint.
type Provider struct {
	BaseURL string `json:"base_url"` // e.g. http://host:1234/v1
	APIKey  string `json:"api_key"`  // optional
	Model   string `json:"model"`
	// Label is an optional short display name for panels; the model
	// ID is what the API actually receives.
	Label string `json:"label"`
	// Anthropic marks the provider as using the Anthropic messages
	// API instead of OpenAI-compatible chat completions.
	Anthropic bool `json:"anthropic"`
}

// ProviderInfo is the panel-facing description of a provider.
type ProviderInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Model string `json:"model"`
}

// DisplayName is the short name to show a human.
func (p ProviderInfo) DisplayName() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Name
}

// Config is the daemon configuration.
type Config struct {
	DefaultProvider string              `json:"default_provider"`
	Providers       map[string]Provider `json:"providers"`

	// MaxScreenBytes caps how much terminal screen context is sent
	// to the model per message. The tail is kept.
	MaxScreenBytes int `json:"max_screen_bytes"`

	// Policy for AI-proposed commands: "ask" (default, always confirm
	// in the terminal), "auto_except_dangerous", "auto_all", or
	// "read_only" (never propose).
	Policy string `json:"policy"`
}

// ExecPolicy returns the normalized command policy.
func (c *Config) ExecPolicy() string {
	switch c.Policy {
	case "auto_except_dangerous", "auto_all", "read_only":
		return c.Policy
	default:
		return "ask"
	}
}

// Path returns the default config file path.
func Path() string {
	if x := os.Getenv("GHOSTTY_AI_CONFIG"); x != "" {
		return x
	}
	cfgHome := os.Getenv("XDG_CONFIG_HOME")
	if cfgHome == "" {
		home, _ := os.UserHomeDir()
		cfgHome = filepath.Join(home, ".config")
	}
	return filepath.Join(cfgHome, "ghostty-ai", "config.json")
}

// Load reads the config file. It is not an error for the file to not
// exist; a config with no providers is returned and the caller reports
// that clearly to panels.
func Load() (*Config, error) {
	cfg := &Config{MaxScreenBytes: 24 * 1024}
	data, err := os.ReadFile(Path())
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", Path(), err)
	}
	if cfg.MaxScreenBytes <= 0 {
		cfg.MaxScreenBytes = 24 * 1024
	}
	return cfg, nil
}

// Default returns the default provider, or false if none is configured.
func (c *Config) Default() (string, Provider, bool) {
	if len(c.Providers) == 0 {
		return "", Provider{}, false
	}
	name := c.DefaultProvider
	p, ok := c.Providers[name]
	if !ok {
		// Deterministic fallback: first sorted key.
		for _, k := range sortedKeys(c.Providers) {
			name, p = k, c.Providers[k]
			break
		}
	}
	return name, p, true
}

// ProviderInfos returns the configured providers, sorted by name, for
// panels to render.
func (c *Config) ProviderInfos() []ProviderInfo {
	names := sortedKeys(c.Providers)
	infos := make([]ProviderInfo, 0, len(names))
	for _, n := range names {
		infos = append(infos, c.Info(n))
	}
	return infos
}

// Info describes one configured provider. An unknown name yields an
// info carrying just the name so panels always have something to show.
func (c *Config) Info(name string) ProviderInfo {
	p, ok := c.Providers[name]
	if !ok {
		return ProviderInfo{Name: name, Label: name}
	}
	return ProviderInfo{Name: name, Label: p.Label, Model: p.Model}
}

func sortedKeys(m map[string]Provider) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple insertion sort; the map is tiny.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
