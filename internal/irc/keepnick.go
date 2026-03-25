package irc

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lrstanley/girc"
)

// DefaultKeepNickInterval is how often to attempt acquiring the desired nick.
const DefaultKeepNickInterval = 30 * time.Second

// KeepNick manages persistent nick acquisition for a single IRC client.
// It periodically attempts to change to the desired nick and also watches
// for QUIT events in case the holder of the nick disconnects.
type KeepNick struct {
	client      *Client
	desiredNick string
	interval    time.Duration
	log         *slog.Logger

	mu         sync.Mutex
	cancel     context.CancelFunc
	active     bool
	acquired   bool
	handlerIDs []string // girc handler IDs for cleanup
}

// NewKeepNick creates a new keepnick manager for the given client.
func NewKeepNick(client *Client, desiredNick string, log *slog.Logger) *KeepNick {
	return &KeepNick{
		client:      client,
		desiredNick: desiredNick,
		interval:    DefaultKeepNickInterval,
		log:         log.With("client_id", client.ID(), "desired_nick", desiredNick),
	}
}

// Start begins the keepnick process. It runs until the nick is acquired,
// the context is cancelled, or Stop is called.
func (kn *KeepNick) Start(ctx context.Context) {
	kn.mu.Lock()
	if kn.active {
		kn.mu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	kn.cancel = cancel
	kn.active = true
	kn.acquired = false
	kn.mu.Unlock()

	kn.log.Info("keepnick started")

	// Register a QUIT handler to detect when the nick holder leaves
	quitCUID := kn.client.GircClient().Handlers.AddBg(girc.QUIT, func(gc *girc.Client, e girc.Event) {
		if e.Source != nil && e.Source.Name == kn.desiredNick {
			kn.log.Info("nick holder quit, attempting nick change")
			kn.attemptNick()
		}
	})

	// Register a NICK handler to detect when someone changes away from the desired nick
	nickCUID := kn.client.GircClient().Handlers.AddBg(girc.NICK, func(gc *girc.Client, e girc.Event) {
		// If the old nick was our desired nick and it wasn't us who changed
		if e.Source != nil && e.Source.Name == kn.desiredNick && e.Source.Name != kn.client.Nick() {
			kn.log.Info("nick holder changed nick, attempting to acquire")
			kn.attemptNick()
		}
		// If we successfully changed to the desired nick
		if gc.GetNick() == kn.desiredNick {
			kn.mu.Lock()
			kn.acquired = true
			kn.mu.Unlock()
			kn.log.Info("desired nick acquired")
		}
	})

	// Store handler IDs for cleanup
	kn.mu.Lock()
	kn.handlerIDs = []string{quitCUID, nickCUID}
	kn.mu.Unlock()

	go func() {
		defer func() {
			// Remove handlers from girc client to prevent leaks
			kn.mu.Lock()
			for _, id := range kn.handlerIDs {
				kn.client.GircClient().Handlers.Remove(id)
			}
			kn.handlerIDs = nil
			kn.active = false
			kn.mu.Unlock()
			kn.log.Debug("keepnick handlers cleaned up")
		}()

		// Attempt immediately
		kn.attemptNick()

		ticker := time.NewTicker(kn.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				kn.log.Info("keepnick stopped")
				return
			case <-ticker.C:
				if kn.IsAcquired() {
					kn.log.Info("nick already acquired, stopping keepnick")
					return
				}
				kn.attemptNick()
			}
		}
	}()
}

// Stop cancels the keepnick process. Handler cleanup happens
// asynchronously when the goroutine exits.
func (kn *KeepNick) Stop() {
	kn.mu.Lock()
	defer kn.mu.Unlock()

	if kn.cancel != nil {
		kn.cancel()
		kn.cancel = nil
	}
	// Note: active will be set to false by the goroutine's defer
}

// IsActive returns whether keepnick is currently running.
func (kn *KeepNick) IsActive() bool {
	kn.mu.Lock()
	defer kn.mu.Unlock()
	return kn.active
}

// IsAcquired returns whether the desired nick has been acquired.
func (kn *KeepNick) IsAcquired() bool {
	kn.mu.Lock()
	defer kn.mu.Unlock()
	return kn.acquired || kn.client.Nick() == kn.desiredNick
}

// DesiredNick returns the nick being sought.
func (kn *KeepNick) DesiredNick() string {
	return kn.desiredNick
}

// attemptNick tries to change to the desired nick.
func (kn *KeepNick) attemptNick() {
	current := kn.client.Nick()
	if current == kn.desiredNick {
		kn.mu.Lock()
		kn.acquired = true
		kn.mu.Unlock()
		return
	}

	kn.log.Debug("attempting nick change", "current", current)
	kn.client.SetNick(kn.desiredNick)
}
