package nick

import (
	"bufio"
	"crypto/rand"
	"embed"
	"fmt"
	"math/big"
	"os"
	"strings"
	"unicode"
)

//go:embed adjectives.txt nouns.txt
var defaultWordlists embed.FS

// nickStyle controls the formatting of a wordlist-generated nick.
type nickStyle int

const (
	// TitleCase: "SwiftFox"
	styleTitleCase nickStyle = iota
	// TitleCase + digits: "SwiftFox42"
	styleTitleCaseDigits
	// lowercase: "swiftfox"
	styleLower
	// lowercase + digits: "swiftfox73"
	styleLowerDigits
	// lower_snake: "swift_fox"
	styleLowerSnake
	// lower_snake + digits: "swift_fox9"
	styleLowerSnakeDigits
)

var allStyles = []nickStyle{
	styleTitleCase,
	styleTitleCaseDigits,
	styleLower,
	styleLowerDigits,
	styleLowerSnake,
	styleLowerSnakeDigits,
}

// wordlistGenerator produces nicks by combining random adjective + noun
// with randomized formatting styles.
type wordlistGenerator struct {
	prefix     string
	maxLength  int
	adjectives []string
	nouns      []string
}

func newWordlistGenerator(prefix string, maxLength int, wordlistPath string) (*wordlistGenerator, error) {
	var adjectives, nouns []string
	var err error

	if wordlistPath != "" {
		adjectives, nouns, err = loadCustomWordlist(wordlistPath)
	} else {
		adjectives, nouns, err = loadEmbeddedWordlists()
	}
	if err != nil {
		return nil, err
	}

	if len(adjectives) == 0 || len(nouns) == 0 {
		return nil, fmt.Errorf("wordlist must contain at least one adjective and one noun")
	}

	return &wordlistGenerator{
		prefix:     prefix,
		maxLength:  maxLength,
		adjectives: adjectives,
		nouns:      nouns,
	}, nil
}

func loadEmbeddedWordlists() (adjectives, nouns []string, err error) {
	adjectives, err = readLines(func() ([]byte, error) {
		return defaultWordlists.ReadFile("adjectives.txt")
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reading embedded adjectives: %w", err)
	}

	nouns, err = readLines(func() ([]byte, error) {
		return defaultWordlists.ReadFile("nouns.txt")
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reading embedded nouns: %w", err)
	}

	return adjectives, nouns, nil
}

func loadCustomWordlist(path string) (adjectives, nouns []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening wordlist %q: %w", path, err)
	}
	defer f.Close()

	// Custom wordlist format: one word per line.
	// First section is adjectives, separated by a blank line, then nouns.
	scanner := bufio.NewScanner(f)
	inNouns := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			if len(adjectives) > 0 {
				inNouns = true
			}
			continue
		}
		// Skip comments.
		if strings.HasPrefix(line, "#") {
			continue
		}
		if inNouns {
			nouns = append(nouns, line)
		} else {
			adjectives = append(adjectives, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading wordlist %q: %w", path, err)
	}
	return adjectives, nouns, nil
}

func readLines(readFn func() ([]byte, error)) ([]string, error) {
	data, err := readFn()
	if err != nil {
		return nil, err
	}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, strings.ToLower(line))
		}
	}
	return lines, scanner.Err()
}

func (g *wordlistGenerator) Generate(_ int) string {
	adj := g.pickRandom(g.adjectives)
	noun := g.pickRandom(g.nouns)
	style := g.pickStyle()

	nick := g.format(adj, noun, style)

	// If prefix is set, prepend it.
	if g.prefix != "" {
		nick = g.prefix + nick
	}

	// Truncate to max length.
	if g.maxLength > 0 && len(nick) > g.maxLength {
		nick = nick[:g.maxLength]
	}

	return nick
}

func (g *wordlistGenerator) pickRandom(words []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return words[0]
	}
	return words[n.Int64()]
}

func (g *wordlistGenerator) pickStyle() nickStyle {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(allStyles))))
	if err != nil {
		return styleLower
	}
	return allStyles[n.Int64()]
}

func (g *wordlistGenerator) format(adj, noun string, style nickStyle) string {
	switch style {
	case styleTitleCase:
		return titleCase(adj) + titleCase(noun)
	case styleTitleCaseDigits:
		return titleCase(adj) + titleCase(noun) + g.randomDigits(1, 3)
	case styleLower:
		return adj + noun
	case styleLowerDigits:
		return adj + noun + g.randomDigits(1, 3)
	case styleLowerSnake:
		return adj + "_" + noun
	case styleLowerSnakeDigits:
		return adj + "_" + noun + g.randomDigits(1, 2)
	default:
		return adj + noun
	}
}

func (g *wordlistGenerator) randomDigits(minLen, maxLen int) string {
	// Pick a random length between minLen and maxLen (inclusive).
	spread := maxLen - minLen + 1
	lenN, err := rand.Int(rand.Reader, big.NewInt(int64(spread)))
	if err != nil {
		return "0"
	}
	length := minLen + int(lenN.Int64())

	digits := make([]byte, length)
	for i := range digits {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			digits[i] = '0'
			continue
		}
		digits[i] = '0' + byte(n.Int64())
	}
	return string(digits)
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
