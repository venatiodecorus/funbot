# Funbot - AI Agent Guidelines

## Project Context

Funbot is a distributed IRC bot system written in Go. It runs as containers on Kubernetes with a controller/worker architecture coordinated via Redis. See `PLAN.md` for the full specification.

## Architecture Summary

- **Single binary, two roles**: `controller` and `worker`, selected via `--role` flag or `FUNBOT_ROLE` env var
- **Controller**: Connects to the home IRC network, accepts commands from an authorized user, routes commands to workers via Redis pub/sub, aggregates status
- **Workers**: One Deployment per IRC network, each pod manages multiple IRC client connections as goroutines, reports status to Redis
- **Redis**: Hub for all cross-node communication (pub/sub for commands, keys for state)

## Code Organization

```
cmd/funbot/main.go          - Entry point, role selection
internal/config/             - YAML + env var config loading via viper
internal/controller/         - Controller logic (commands, context, status, scaling)
internal/worker/             - Worker logic (client management, command execution)
internal/irc/                - IRC client wrapper around girc library
internal/art/                - ASCII art repo management & playback
internal/proxy/              - SOCKS5 proxy pool management
internal/redis/              - Redis client, pub/sub, state helpers
internal/auth/               - Nick+hostname authorization
deploy/                      - Dockerfile, docker-compose, k8s manifests
config/funbot.yaml           - Default configuration
```

## Key Conventions

### Go Style
- Follow standard Go conventions (gofmt, golint)
- Use `log/slog` for all logging (structured, JSON format)
- Use `context.Context` for cancellation and timeouts throughout
- Errors should be wrapped with `fmt.Errorf("doing X: %w", err)` for context
- No channel logging -- only log bot operational events

### Naming
- IRC networks are identified by short string keys (e.g., "efnet", "undernet", "homenet")
- Client IDs follow the format `<network>-<index>` (e.g., "efnet-0", "efnet-1")
- Redis keys follow the pattern `funbot:<type>:<scope>` (e.g., `funbot:state:efnet:pod-abc123`)

### Configuration
- Base config is in YAML (`config/funbot.yaml`)
- Environment variables override config values with `FUNBOT_` prefix
- Viper is used for config loading
- Runtime-added networks are NOT persisted -- only default config networks survive restarts

### IRC Commands
- All commands are prefixed with `!` (configurable)
- Commands support an optional context system: user can set a default network/channel
- When context is set, network/channel arguments become optional
- Explicit arguments always override context
- Commands are only accepted from an authorized user (nick+hostname match)

### Testing
- Unit tests for config parsing, command parsing, auth, art distribution
- Integration tests use `ergo` IRCd in docker-compose
- No mocking of IRC connections in unit tests -- test the logic layers instead
- Run tests with `make test`

### Docker & Deployment
- Multi-stage Dockerfile: golang:1.22-alpine builder, distroless runtime
- Local dev uses docker-compose (Redis + controller + workers)
- Production uses Kubernetes (see deploy/k8s/)
- The controller needs RBAC permissions to scale worker Deployments

## Progress Tracking

Build progress is tracked in `PLAN.md` with checkbox items under Phase 1-6. When completing a task, mark the corresponding checkbox as `[x]`.

## Common Tasks

### Adding a new command
1. Add the command handler in `internal/controller/commands.go`
2. Register it in the command dispatcher
3. If it targets workers, define the Redis message format
4. Add the executor in `internal/worker/executor.go`
5. Update the command table in `PLAN.md` section 6.2

### Adding a new network config option
1. Add the field to the network config struct in `internal/config/config.go`
2. Add a default value
3. Document it in `config/funbot.yaml`
4. Update `PLAN.md` section 5.1

### Modifying the IRC client
- All IRC interaction goes through `internal/irc/client.go` which wraps `girc`
- Never use `girc` directly outside of the `internal/irc` package
- Flood control is handled in `internal/irc/flood.go`
