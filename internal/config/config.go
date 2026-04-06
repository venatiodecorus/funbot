// Package config handles loading and validating Funbot configuration
// from YAML files and environment variables.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// NickStrategy identifies a nick generation strategy.
type NickStrategy string

const (
	NickStrategyPrefix   NickStrategy = "prefix"
	NickStrategyRandom   NickStrategy = "random"
	NickStrategyWordlist NickStrategy = "wordlist"
)

// NickConfig holds nick generation settings for a network.
type NickConfig struct {
	Strategy     NickStrategy `mapstructure:"strategy"`
	Prefix       string       `mapstructure:"prefix"`
	Length       int          `mapstructure:"length"`
	WordlistPath string       `mapstructure:"wordlist_path"`
}

// Config is the top-level configuration for Funbot.
type Config struct {
	HomeNetwork   string             `mapstructure:"home_network"`
	Auth          AuthConfig         `mapstructure:"auth"`
	CommandPrefix string             `mapstructure:"command_prefix"`
	Networks      map[string]Network `mapstructure:"networks"`
	Proxies       ProxyConfig        `mapstructure:"proxies"`
	Art           ArtConfig          `mapstructure:"art"`
	Logging       LoggingConfig      `mapstructure:"logging"`
}

// AuthConfig specifies the authorized user for issuing commands.
type AuthConfig struct {
	Nick     string `mapstructure:"nick"`
	Hostname string `mapstructure:"hostname"`
}

// Network holds configuration for a single IRC network.
type Network struct {
	Servers        []string   `mapstructure:"servers"`
	SSL            bool       `mapstructure:"ssl"`
	NickPrefix     string     `mapstructure:"nick_prefix"` // Legacy: used as fallback if Nick is not set
	Nick           NickConfig `mapstructure:"nick"`        // Nick generation config
	Channels       []string   `mapstructure:"channels"`
	FloodDelayMs   int        `mapstructure:"flood_delay_ms"`
	DefaultClients int        `mapstructure:"default_clients"`
}

// EffectiveNickConfig returns the nick generation config, applying backward
// compatibility with the legacy nick_prefix field. If the Nick block is not
// configured, it falls back to using NickPrefix with the "prefix" strategy.
func (n Network) EffectiveNickConfig() NickConfig {
	// If the new Nick config has an explicit strategy, use it.
	if n.Nick.Strategy != "" {
		return n.Nick
	}
	// Fall back to legacy nick_prefix field.
	return NickConfig{
		Strategy: NickStrategyPrefix,
		Prefix:   n.NickPrefix,
	}
}

// FloodDelay returns the flood delay as a time.Duration.
func (n Network) FloodDelay() time.Duration {
	return time.Duration(n.FloodDelayMs) * time.Millisecond
}

// ProxyConfig holds proxy API configuration.
type ProxyConfig struct {
	// APIURL is the base URL of the proxy-scanner API (e.g. "http://localhost:8080").
	APIURL string `mapstructure:"api_url"`
	// Protocol filters which proxy protocol to request (e.g. "socks5", "socks4").
	Protocol string `mapstructure:"protocol"`
	// MaxLatency filters proxies by maximum latency in milliseconds.
	MaxLatency int `mapstructure:"max_latency"`
	// RefreshInterval is how often to re-fetch proxies from the API.
	RefreshInterval string `mapstructure:"refresh_interval"`
	// MaxRetries is the number of consecutive connection failures before a proxy
	// is purged from the pool entirely. 0 means purge on first failure (legacy
	// behavior). Default is 3.
	MaxRetries int `mapstructure:"max_retries"`
	// MinPoolSize is the minimum number of healthy proxies to maintain in the
	// pool. When the healthy count drops below this threshold, the pool
	// automatically fetches more from the API during the background refresh
	// cycle. 0 means no minimum (on-demand fetching only).
	MinPoolSize int `mapstructure:"min_pool_size"`
}

// ArtConfig holds ASCII art repository configuration.
type ArtConfig struct {
	RepoURL        string `mapstructure:"repo_url"`
	LocalPath      string `mapstructure:"local_path"`
	UpdateInterval string `mapstructure:"update_interval"`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
}

// Load reads configuration from the given file path, then overlays
// environment variables with the FUNBOT_ prefix.
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("command_prefix", "!")
	v.SetDefault("art.repo_url", "https://github.com/birdneststream/asciiart.git")
	v.SetDefault("art.local_path", "/data/asciiart")
	v.SetDefault("art.update_interval", "1h")
	v.SetDefault("proxies.max_retries", 3)
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")

	// Read config file
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("funbot")
		v.SetConfigType("yaml")
		v.AddConfigPath("/etc/funbot")
		v.AddConfigPath("./config")
		v.AddConfigPath(".")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		// Config file not found is ok — we'll use defaults + env vars
	}

	// Environment variable overrides
	v.SetEnvPrefix("FUNBOT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// Validate checks that the configuration is consistent and usable.
func (c *Config) Validate() error {
	if c.HomeNetwork == "" {
		return fmt.Errorf("home_network is required")
	}

	if c.Auth.Nick == "" || c.Auth.Hostname == "" {
		return fmt.Errorf("auth.nick and auth.hostname are required")
	}

	if _, ok := c.Networks[c.HomeNetwork]; !ok {
		return fmt.Errorf("home_network %q not found in networks", c.HomeNetwork)
	}

	for name, net := range c.Networks {
		if len(net.Servers) == 0 {
			return fmt.Errorf("network %q has no servers configured", name)
		}
		// Validate nick config. Either the new nick block or legacy nick_prefix must be set.
		nickCfg := net.EffectiveNickConfig()
		switch nickCfg.Strategy {
		case NickStrategyPrefix:
			if nickCfg.Prefix == "" && net.NickPrefix == "" {
				return fmt.Errorf("network %q: prefix nick strategy requires a non-empty prefix (set nick.prefix or nick_prefix)", name)
			}
		case NickStrategyRandom:
			// Random strategy works with or without a prefix.
		case NickStrategyWordlist:
			// Wordlist strategy works with or without a prefix.
		case "":
			// No strategy set and no legacy nick_prefix — error.
			if net.NickPrefix == "" {
				return fmt.Errorf("network %q has no nick configuration (set nick.strategy or nick_prefix)", name)
			}
		default:
			return fmt.Errorf("network %q: unknown nick strategy %q", name, nickCfg.Strategy)
		}
	}

	return nil
}
