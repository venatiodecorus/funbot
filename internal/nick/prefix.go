package nick

import "fmt"

// prefixGenerator produces nicks in the form prefix+index (e.g., "fun0", "fun1").
// This is the original/legacy behavior.
type prefixGenerator struct {
	prefix string
}

func newPrefixGenerator(prefix string) *prefixGenerator {
	return &prefixGenerator{prefix: prefix}
}

func (g *prefixGenerator) Generate(index int) string {
	return fmt.Sprintf("%s%d", g.prefix, index)
}
