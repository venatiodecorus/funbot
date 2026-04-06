package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "funbot.yaml")

	content := `
home_network: homenet
command_prefix: "!"
auth:
  nick: admin
  hostname: admin.host.com
networks:
  homenet:
    servers:
      - "irc.test.net:6667"
    nick_prefix: bot
    channels:
      - "#test"
    flood_delay_ms: 500
    default_clients: 3
logging:
  level: debug
  format: text
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.HomeNetwork != "homenet" {
		t.Errorf("expected home_network 'homenet', got %q", cfg.HomeNetwork)
	}
	if cfg.Auth.Nick != "admin" {
		t.Errorf("expected auth nick 'admin', got %q", cfg.Auth.Nick)
	}
	if cfg.CommandPrefix != "!" {
		t.Errorf("expected command_prefix '!', got %q", cfg.CommandPrefix)
	}

	net, ok := cfg.Networks["homenet"]
	if !ok {
		t.Fatal("expected 'homenet' in networks")
	}
	if len(net.Servers) != 1 || net.Servers[0] != "irc.test.net:6667" {
		t.Errorf("unexpected servers: %v", net.Servers)
	}
	if net.FloodDelayMs != 500 {
		t.Errorf("expected flood_delay_ms 500, got %d", net.FloodDelayMs)
	}
	if net.DefaultClients != 3 {
		t.Errorf("expected default_clients 3, got %d", net.DefaultClients)
	}
}

func TestLoad_MissingHomeNetwork(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "funbot.yaml")

	content := `
home_network: nonexistent
auth:
  nick: admin
  hostname: admin.host.com
networks:
  homenet:
    servers:
      - "irc.test.net:6667"
    nick_prefix: bot
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing home network")
	}
}

func TestLoad_MissingAuth(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "funbot.yaml")

	content := `
home_network: homenet
networks:
  homenet:
    servers:
      - "irc.test.net:6667"
    nick_prefix: bot
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing auth")
	}
}

func TestNetwork_FloodDelay(t *testing.T) {
	n := Network{FloodDelayMs: 500}
	d := n.FloodDelay()
	if d.Milliseconds() != 500 {
		t.Errorf("expected 500ms, got %v", d)
	}
}

func TestLoad_MissingNickPrefix(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "funbot.yaml")

	content := `
home_network: homenet
auth:
  nick: admin
  hostname: admin.host.com
networks:
  homenet:
    servers:
      - "irc.test.net:6667"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing nick_prefix")
	}
}
