// Package config handles loading and validating Funbot configuration
// from YAML files and environment variables.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

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
	Servers        []string `mapstructure:"servers"`
	SSL            bool     `mapstructure:"ssl"`
	NickPrefix     string   `mapstructure:"nick_prefix"`
	Channels       []string `mapstructure:"channels"`
	FloodDelayMs   int      `mapstructure:"flood_delay_ms"`
	DefaultClients int      `mapstructure:"default_clients"`
}

// FloodDelay returns the flood delay as a time.Duration.
func (n Network) FloodDelay() time.Duration {
	return time.Duration(n.FloodDelayMs) * time.Millisecond
}

// ProxyConfig holds proxy list configuration.
type ProxyConfig struct {
	File string   `mapstructure:"file"`
	List []string `mapstructure:"list"`
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
		if net.NickPrefix == "" {
			return fmt.Errorf("network %q has no nick_prefix configured", name)
		}
	}

	return nil
}
