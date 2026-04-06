package nick

import (
	"regexp"
	"strings"
	"testing"
)

// --- Factory tests ---

func TestNewGenerator_DefaultsToPrefix(t *testing.T) {
	g, err := NewGenerator(Config{Prefix: "bot"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	nick := g.Generate(0)
	if nick != "bot0" {
		t.Errorf("expected bot0, got %q", nick)
	}
}

func TestNewGenerator_UnknownStrategy(t *testing.T) {
	_, err := NewGenerator(Config{Strategy: "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

func TestNewGenerator_PrefixRequiresPrefix(t *testing.T) {
	_, err := NewGenerator(Config{Strategy: StrategyPrefix, Prefix: ""})
	if err == nil {
		t.Fatal("expected error for empty prefix")
	}
}

// --- Prefix generator ---

func TestPrefixGenerator(t *testing.T) {
	g := newPrefixGenerator("fun")
	tests := []struct {
		index int
		want  string
	}{
		{0, "fun0"},
		{1, "fun1"},
		{99, "fun99"},
	}
	for _, tt := range tests {
		got := g.Generate(tt.index)
		if got != tt.want {
			t.Errorf("Generate(%d) = %q, want %q", tt.index, got, tt.want)
		}
	}
}

// --- Random generator ---

func TestRandomGenerator_Length(t *testing.T) {
	g := newRandomGenerator("", 10)
	for i := 0; i < 50; i++ {
		nick := g.Generate(i)
		if len(nick) != 10 {
			t.Errorf("expected length 10, got %d for %q", len(nick), nick)
		}
	}
}

func TestRandomGenerator_WithPrefix(t *testing.T) {
	g := newRandomGenerator("pre", 10)
	for i := 0; i < 50; i++ {
		nick := g.Generate(i)
		if !strings.HasPrefix(nick, "pre") {
			t.Errorf("expected prefix 'pre', got %q", nick)
		}
		if len(nick) != 10 {
			t.Errorf("expected length 10, got %d for %q", len(nick), nick)
		}
	}
}

func TestRandomGenerator_StartsWithLetter(t *testing.T) {
	g := newRandomGenerator("", 12)
	letterRe := regexp.MustCompile(`^[a-zA-Z]`)
	for i := 0; i < 100; i++ {
		nick := g.Generate(i)
		if !letterRe.MatchString(nick) {
			t.Errorf("nick %q does not start with a letter", nick)
		}
	}
}

func TestRandomGenerator_Uniqueness(t *testing.T) {
	g, err := NewGenerator(Config{Strategy: StrategyRandom, Length: 12})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := make(map[string]struct{})
	for i := 0; i < 200; i++ {
		nick := g.Generate(i)
		if _, exists := seen[nick]; exists {
			t.Errorf("duplicate nick %q at iteration %d", nick, i)
		}
		seen[nick] = struct{}{}
	}
}

// --- Wordlist generator ---

func TestWordlistGenerator_EmbeddedLoads(t *testing.T) {
	g, err := newWordlistGenerator("", 16, "")
	if err != nil {
		t.Fatalf("failed to load embedded wordlists: %v", err)
	}
	if len(g.adjectives) == 0 {
		t.Error("no adjectives loaded")
	}
	if len(g.nouns) == 0 {
		t.Error("no nouns loaded")
	}
}

func TestWordlistGenerator_ProducesValidNicks(t *testing.T) {
	g, err := NewGenerator(Config{Strategy: StrategyWordlist, Length: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// IRC nicks must start with a letter and contain only [a-zA-Z0-9_\-\[\]\\`^{}|]
	// Our wordlist nicks use letters, digits, and underscores.
	validRe := regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
	for i := 0; i < 200; i++ {
		nick := g.Generate(i)
		if !validRe.MatchString(nick) {
			t.Errorf("invalid IRC nick %q", nick)
		}
		if len(nick) > 20 {
			t.Errorf("nick %q exceeds max length 20", nick)
		}
	}
}

func TestWordlistGenerator_StyleVariation(t *testing.T) {
	g, err := newWordlistGenerator("", 30, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hasUpper := false
	hasAllLower := false
	hasUnderscore := false
	hasDigits := false

	digitRe := regexp.MustCompile(`\d`)

	for i := 0; i < 500; i++ {
		nick := g.Generate(i)
		if strings.Contains(nick, "_") {
			hasUnderscore = true
		}
		if nick == strings.ToLower(nick) && !strings.Contains(nick, "_") {
			hasAllLower = true
		}
		if nick != strings.ToLower(nick) {
			hasUpper = true
		}
		if digitRe.MatchString(nick) {
			hasDigits = true
		}
	}

	if !hasUpper {
		t.Error("no TitleCase nicks produced in 500 iterations")
	}
	if !hasAllLower {
		t.Error("no all-lowercase nicks produced in 500 iterations")
	}
	if !hasUnderscore {
		t.Error("no underscore (snake_case) nicks produced in 500 iterations")
	}
	if !hasDigits {
		t.Error("no nicks with digit suffixes produced in 500 iterations")
	}
}

func TestWordlistGenerator_WithPrefix(t *testing.T) {
	g, err := NewGenerator(Config{
		Strategy: StrategyWordlist,
		Prefix:   "z",
		Length:   20,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 50; i++ {
		nick := g.Generate(i)
		if !strings.HasPrefix(nick, "z") {
			t.Errorf("expected prefix 'z', got %q", nick)
		}
	}
}

func TestWordlistGenerator_MaxLengthRespected(t *testing.T) {
	g, err := NewGenerator(Config{
		Strategy: StrategyWordlist,
		Length:   9,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 200; i++ {
		nick := g.Generate(i)
		if len(nick) > 9 {
			t.Errorf("nick %q exceeds max length 9", nick)
		}
	}
}

func TestWordlistGenerator_Uniqueness(t *testing.T) {
	g, err := NewGenerator(Config{Strategy: StrategyWordlist, Length: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := make(map[string]struct{})
	for i := 0; i < 200; i++ {
		nick := g.Generate(i)
		if _, exists := seen[nick]; exists {
			t.Errorf("duplicate nick %q at iteration %d", nick, i)
		}
		seen[nick] = struct{}{}
	}
}

// --- Deduplicator ---

func TestDeduplicator_NoDuplicates(t *testing.T) {
	// Use a prefix generator which is deterministic, wrap it in dedup.
	inner := newPrefixGenerator("x")
	d := newDeduplicator(inner, 10)

	// Calling with the same index should still get unique results
	// (first call returns "x0", second call also tries "x0" but gets fallback).
	n1 := d.Generate(0)
	n2 := d.Generate(0)
	if n1 == n2 {
		t.Errorf("deduplicator returned duplicate: %q", n1)
	}
}
