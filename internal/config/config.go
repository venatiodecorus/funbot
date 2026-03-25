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
	Role       string             `mapstructure:"role"`
	Controller ControllerConfig   `mapstructure:"controller"`
	Redis      RedisConfig        `mapstructure:"redis"`
	Networks   map[string]Network `mapstructure:"networks"`
	Proxies    ProxyConfig        `mapstructure:"proxies"`
	Art        ArtConfig          `mapstructure:"art"`
	Logging    LoggingConfig      `mapstructure:"logging"`
}

// ControllerConfig holds settings specific to the controller role.
type ControllerConfig struct {
	HomeNetwork   string     `mapstructure:"home_network"`
	Auth          AuthConfig `mapstructure:"auth"`
	CommandPrefix string     `mapstructure:"command_prefix"`
}

// AuthConfig specifies the authorized user for issuing commands.
type AuthConfig struct {
	Nick     string `mapstructure:"nick"`
	Hostname string `mapstructure:"hostname"`
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Address  string `mapstructure:"address"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// Network holds configuration for a single IRC network.
type Network struct {
	Servers         []string `mapstructure:"servers"`
	SSL             bool     `mapstructure:"ssl"`
	NickPrefix      string   `mapstructure:"nick_prefix"`
	MaxClientsPerIP int      `mapstructure:"max_clients_per_ip"`
	Channels        []string `mapstructure:"channels"`
	FloodDelayMs    int      `mapstructure:"flood_delay_ms"`
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
	v.SetDefault("role", "controller")
	v.SetDefault("controller.command_prefix", "!")
	v.SetDefault("redis.address", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)
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
	if c.Role != "controller" && c.Role != "worker" {
		return fmt.Errorf("role must be 'controller' or 'worker', got %q", c.Role)
	}

	if c.Role == "controller" {
		if c.Controller.HomeNetwork == "" {
			return fmt.Errorf("controller.home_network is required for controller role")
		}
		if c.Controller.Auth.Nick == "" || c.Controller.Auth.Hostname == "" {
			return fmt.Errorf("controller.auth.nick and controller.auth.hostname are required")
		}
		if _, ok := c.Networks[c.Controller.HomeNetwork]; !ok {
			return fmt.Errorf("controller.home_network %q not found in networks", c.Controller.HomeNetwork)
		}
	}

	for name, net := range c.Networks {
		if len(net.Servers) == 0 {
			return fmt.Errorf("network %q has no servers configured", name)
		}
		if net.NickPrefix == "" {
			return fmt.Errorf("network %q has no nick_prefix configured", name)
		}
		if net.MaxClientsPerIP <= 0 {
			return fmt.Errorf("network %q max_clients_per_ip must be > 0", name)
		}
	}

	return nil
}
