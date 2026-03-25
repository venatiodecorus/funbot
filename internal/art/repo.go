// Package art manages ASCII art repository cloning, indexing, and playback.
package art

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Repo manages a git clone of the ASCII art repository, with periodic
// background updates.
type Repo struct {
	repoURL   string
	localPath string
	interval  time.Duration
	log       *slog.Logger

	mu       sync.RWMutex
	ready    bool
	lastPull time.Time
}

// NewRepo creates a new art repository manager.
func NewRepo(repoURL, localPath string, interval time.Duration, log *slog.Logger) *Repo {
	return &Repo{
		repoURL:   repoURL,
		localPath: localPath,
		interval:  interval,
		log:       log.With("component", "art-repo"),
	}
}

// Init performs the initial clone or pull of the art repository.
// This should be called before starting the background updater.
func (r *Repo) Init(ctx context.Context) error {
	if r.repoURL == "" {
		return fmt.Errorf("art repo URL is empty")
	}

	if _, err := os.Stat(r.localPath); os.IsNotExist(err) {
		r.log.Info("cloning art repo", "url", r.repoURL, "path", r.localPath)
		if err := r.gitClone(ctx); err != nil {
			return fmt.Errorf("cloning art repo: %w", err)
		}
	} else {
		r.log.Info("art repo exists, pulling updates", "path", r.localPath)
		if err := r.gitPull(ctx); err != nil {
			r.log.Warn("initial pull failed, using existing repo", "error", err)
		}
	}

	r.mu.Lock()
	r.ready = true
	r.lastPull = time.Now()
	r.mu.Unlock()

	return nil
}

// StartUpdater runs a background goroutine that periodically pulls
// updates from the art repository. It blocks until the context is cancelled.
func (r *Repo) StartUpdater(ctx context.Context) {
	if r.interval <= 0 {
		r.log.Info("art repo updater disabled (interval <= 0)")
		return
	}

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.log.Info("art repo updater started", "interval", r.interval)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("art repo updater stopped")
			return
		case <-ticker.C:
			if err := r.Pull(ctx); err != nil {
				r.log.Error("periodic art repo pull failed", "error", err)
			}
		}
	}
}

// Pull fetches the latest changes from the remote repository.
func (r *Repo) Pull(ctx context.Context) error {
	r.log.Debug("pulling art repo updates")

	if err := r.gitPull(ctx); err != nil {
		return fmt.Errorf("pulling art repo: %w", err)
	}

	r.mu.Lock()
	r.lastPull = time.Now()
	r.mu.Unlock()

	r.log.Info("art repo updated", "time", r.lastPull)
	return nil
}

// IsReady returns whether the repository has been successfully initialized.
func (r *Repo) IsReady() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready
}

// LocalPath returns the local filesystem path of the repository.
func (r *Repo) LocalPath() string {
	return r.localPath
}

// LastPull returns the time of the last successful pull.
func (r *Repo) LastPull() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastPull
}

// gitClone runs git clone to create the local repository.
func (r *Repo) gitClone(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", r.repoURL, r.localPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		r.log.Error("git clone failed", "error", err, "output", string(output))
		return err
	}
	r.log.Debug("git clone completed", "output", string(output))
	return nil
}

// gitPull runs git pull in the local repository.
func (r *Repo) gitPull(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "git", "-C", r.localPath, "pull", "--ff-only")
	output, err := cmd.CombinedOutput()
	if err != nil {
		r.log.Error("git pull failed", "error", err, "output", string(output))
		return err
	}
	r.log.Debug("git pull completed", "output", string(output))
	return nil
}
