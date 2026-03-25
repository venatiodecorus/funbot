// Package auth provides nick+hostname authorization checking
// for incoming IRC commands.
package auth

// Checker validates whether an IRC user is authorized to issue commands.
type Checker struct {
	nick     string
	hostname string
}

// New creates a new auth Checker with the given authorized nick and hostname.
func New(nick, hostname string) *Checker {
	return &Checker{
		nick:     nick,
		hostname: hostname,
	}
}

// IsAuthorized checks if the given nick and hostname match the authorized user.
func (c *Checker) IsAuthorized(nick, hostname string) bool {
	return c.nick == nick && c.hostname == hostname
}
