// Package redis provides Redis client setup and helpers for Funbot's
// cross-node communication via pub/sub and state keys.
package redis

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
	"github.com/venatiodecorus/funbot/internal/config"
)

// Client wraps a go-redis client with Funbot-specific helpers.
type Client struct {
	rdb *redis.Client
	log *slog.Logger
}

// New creates a new Redis client from the given config.
func New(cfg config.RedisConfig, log *slog.Logger) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Address,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	return &Client{
		rdb: rdb,
		log: log.With("component", "redis"),
	}, nil
}

// Ping checks the Redis connection.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("pinging redis: %w", err)
	}
	return nil
}

// Close closes the Redis connection.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Underlying returns the raw go-redis client for advanced operations.
func (c *Client) Underlying() *redis.Client {
	return c.rdb
}
