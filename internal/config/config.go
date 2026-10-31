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
	// Anthropic marks the provider as using the Anthropic messages
	// API instead of OpenAI-compatible chat completions.
	Anthropic bool `json:"anthropic"`
}

// Config is the daemon configuration.
type Config struct {
	DefaultProvider string              `json:"default_provider"`
	Providers       map[string]Provider `json:"providers"`

	// MaxScreenBytes caps how much terminal screen context is sent
	// to the model per message. The tail is kept.
	MaxScreenBytes int `json:"max_screen_bytes"`
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
