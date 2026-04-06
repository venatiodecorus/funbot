package nick

import (
	"crypto/rand"
	"math/big"
)

const alphanumeric = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// IRC nicks must start with a letter, so we use a separate alphabet for the first char.
const alphaOnly = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// randomGenerator produces random alphanumeric nicks with an optional prefix.
type randomGenerator struct {
	prefix string
	length int // total max nick length including prefix
}

func newRandomGenerator(prefix string, length int) *randomGenerator {
	return &randomGenerator{
		prefix: prefix,
		length: length,
	}
}

func (g *randomGenerator) Generate(_ int) string {
	randLen := g.length - len(g.prefix)
	if randLen <= 0 {
		randLen = 6
	}

	buf := make([]byte, randLen)
	for i := range buf {
		var charset string
		// First character of the nick must be a letter (if no prefix).
		if i == 0 && g.prefix == "" {
			charset = alphaOnly
		} else {
			charset = alphanumeric
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			// Fallback to a predictable but valid char.
			buf[i] = 'x'
			continue
		}
		buf[i] = charset[n.Int64()]
	}
	return g.prefix + string(buf)
}
