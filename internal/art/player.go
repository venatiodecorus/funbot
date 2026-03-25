package art

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/venatiodecorus/funbot/internal/irc"
)

// Player handles loading and coordinated playback of ASCII art files
// across one or more IRC clients.
type Player struct {
	floodDelay time.Duration
	floodGuard *irc.GlobalFloodGuard // optional global rate limit
	log        *slog.Logger
}

// NewPlayer creates a new art player with the specified flood delay.
// An optional GlobalFloodGuard can be provided to enforce network-wide rate limits.
func NewPlayer(floodDelay time.Duration, floodGuard *irc.GlobalFloodGuard, log *slog.Logger) *Player {
	return &Player{
		floodDelay: floodDelay,
		floodGuard: floodGuard,
		log:        log.With("component", "art-player"),
	}
}

// LoadArt reads an art file and returns its lines. Each line is sent
// as-is (preserving mIRC color codes) via PRIVMSG.
func LoadArt(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening art file %s: %w", path, err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	// Increase scanner buffer for art files that may have very long lines
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading art file %s: %w", path, err)
	}

	return lines, nil
}

// Play performs coordinated playback of art lines to a channel using
// the provided clients. It implements the round-robin distribution
// algorithm from the spec:
//
//   - Lines are assigned to clients round-robin: client 0 gets lines
//     0, N, 2N...; client 1 gets lines 1, N+1, 2N+1...
//   - A conductor goroutine signals each client to send its next line
//     in sequence, respecting the flood delay.
//   - With N clients, effective inter-line delay = floodDelay / N.
//
// Play blocks until all lines are sent or the context is cancelled.
func (p *Player) Play(ctx context.Context, channel string, clients []*irc.Client, lines []string) error {
	if len(clients) == 0 {
		return fmt.Errorf("no clients available for playback")
	}
	if len(lines) == 0 {
		return nil
	}

	numClients := len(clients)
	numLines := len(lines)

	p.log.Info("starting art playback",
		"channel", channel,
		"clients", numClients,
		"lines", numLines,
		"flood_delay", p.floodDelay,
	)

	// With N clients, each client sends every Nth line.
	// The effective delay between consecutive lines (across all clients)
	// is floodDelay / N, since each individual client respects floodDelay
	// between its own messages.
	//
	// The conductor sends lines in order: line 0 via client 0, line 1
	// via client 1, ..., line N-1 via client N-1, line N via client 0, etc.

	// Effective inter-line delay for the conductor
	interLineDelay := p.floodDelay
	if numClients > 1 {
		interLineDelay = p.floodDelay / time.Duration(numClients)
	}

	// Minimum inter-line delay to avoid near-zero delays with many clients
	const minInterLineDelay = 50 * time.Millisecond
	if interLineDelay < minInterLineDelay {
		interLineDelay = minInterLineDelay
	}

	// Per-client flood trackers to ensure each client respects its own delay
	type clientTracker struct {
		client   *irc.Client
		lastSend time.Time
	}
	trackers := make([]clientTracker, numClients)
	for i, c := range clients {
		trackers[i] = clientTracker{client: c}
	}

	// Conductor loop: send lines round-robin across clients
	for lineIdx := 0; lineIdx < numLines; lineIdx++ {
		select {
		case <-ctx.Done():
			p.log.Info("art playback cancelled", "lines_sent", lineIdx, "total", numLines)
			return ctx.Err()
		default:
		}

		clientIdx := lineIdx % numClients
		tracker := &trackers[clientIdx]
		line := lines[lineIdx]

		// Enforce per-client flood delay
		if !tracker.lastSend.IsZero() {
			elapsed := time.Since(tracker.lastSend)
			if elapsed < p.floodDelay {
				wait := p.floodDelay - elapsed
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
		}

		// Also enforce inter-line delay (for smooth playback ordering)
		if lineIdx > 0 {
			// The inter-line delay is already partially covered by the
			// per-client delay above for the same client, but for different
			// clients we need a short delay to maintain ordering
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interLineDelay):
			}
		}

		// Acquire global flood guard token if available
		if p.floodGuard != nil {
			p.floodGuard.Acquire()
		}

		tracker.client.PrivmsgNoFlood(channel, line)
		tracker.lastSend = time.Now()
	}

	p.log.Info("art playback complete", "lines_sent", numLines, "channel", channel)
	return nil
}
