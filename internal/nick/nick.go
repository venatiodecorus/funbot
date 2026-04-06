// Package nick provides IRC nick generation strategies.
package nick

import (
	"fmt"
	"sync"
)

// Strategy identifies a nick generation strategy.
type Strategy string

const (
	StrategyPrefix   Strategy = "prefix"
	StrategyRandom   Strategy = "random"
	StrategyWordlist Strategy = "wordlist"
)

// Config holds nick generation settings for a network.
type Config struct {
	Strategy     Strategy `mapstructure:"strategy"`
	Prefix       string   `mapstructure:"prefix"`
	Length       int      `mapstructure:"length"`
	WordlistPath string   `mapstructure:"wordlist_path"`
}

// Generator produces unique nicks for IRC clients.
type Generator interface {
	// Generate returns a nick for the given client index.
	// The index is provided so deterministic strategies (like prefix) can use it.
	Generate(index int) string
}

// deduplicator wraps a Generator and ensures no duplicate nicks are produced
// within the lifetime of the wrapper.
type deduplicator struct {
	inner    Generator
	seen     map[string]struct{}
	mu       sync.Mutex
	maxTries int
}

func newDeduplicator(inner Generator, maxTries int) *deduplicator {
	return &deduplicator{
		inner:    inner,
		seen:     make(map[string]struct{}),
		maxTries: maxTries,
	}
}

func (d *deduplicator) Generate(index int) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	for i := 0; i < d.maxTries; i++ {
		nick := d.inner.Generate(index)
		if _, exists := d.seen[nick]; !exists {
			d.seen[nick] = struct{}{}
			return nick
		}
	}
	// Fallback: append index to force uniqueness.
	nick := fmt.Sprintf("%s%d", d.inner.Generate(index), index)
	d.seen[nick] = struct{}{}
	return nick
}

// NewGenerator creates a Generator for the given config.
// The returned generator guarantees unique nicks within a process.
func NewGenerator(cfg Config) (Generator, error) {
	// Apply defaults.
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyPrefix
	}
	if cfg.Length <= 0 {
		cfg.Length = 9
	}

	var g Generator
	var err error

	switch cfg.Strategy {
	case StrategyPrefix:
		if cfg.Prefix == "" {
			return nil, fmt.Errorf("nick: prefix strategy requires a non-empty prefix")
		}
		g = newPrefixGenerator(cfg.Prefix)
		// Prefix generator is deterministic; no dedup needed.
		return g, nil

	case StrategyRandom:
		g = newRandomGenerator(cfg.Prefix, cfg.Length)

	case StrategyWordlist:
		g, err = newWordlistGenerator(cfg.Prefix, cfg.Length, cfg.WordlistPath)
		if err != nil {
			return nil, fmt.Errorf("nick: creating wordlist generator: %w", err)
		}

	default:
		return nil, fmt.Errorf("nick: unknown strategy %q", cfg.Strategy)
	}

	return newDeduplicator(g, 100), nil
}
