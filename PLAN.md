# Funbot - Project Plan & Specification

## 1. Project Overview

**Funbot** is a distributed IRC bot system written in Go, deployed as containers on Kubernetes. It provides centralized control over multiple IRC client connections across multiple IRC networks, with features including multi-client coordination, ASCII art playback, proxy support, and dynamic scaling.

## 2. Architecture

### 2.1 Node Roles

There are two logical roles, both running the same binary with different configuration:

- **Controller**: A single-replica Deployment. Connects to the "home" IRC network. Accepts commands from the authorized user. Coordinates all workers via Redis. Aggregates and reports status.
- **Worker**: One Deployment per IRC network (scalable replicas). Each pod manages N IRC client connections (goroutines) to its assigned network. Reports status to Redis. Receives commands from Redis pub/sub.

The same Go binary serves both roles, selected by a `--role=controller|worker` flag (or `FUNBOT_ROLE` env var).

### 2.2 Communication & State (Redis)

```
┌──────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                     │
│                                                          │
│  ┌─────────────────┐         ┌────────────┐             │
│  │   Controller    │◄───────►│   Redis    │             │
│  │  (home network) │  r/w    │            │             │
│  └─────────────────┘         └─────┬──────┘             │
│                                    │                     │
│              ┌─────────────────────┼──────────┐         │
│              │                     │          │         │
│         ┌────▼────┐          ┌────▼────┐ ┌───▼────┐   │
│         │ Worker  │          │ Worker  │ │ Worker │   │
│         │ EFnet   │          │ EFnet   │ │ Undernet│   │
│         │ Pod 1   │          │ Pod 2   │ │ Pod 1  │   │
│         │ (3 cli) │          │ (3 cli) │ │ (2 cli)│   │
│         └─────────┘          └─────────┘ └────────┘   │
│                                                          │
│  ┌──────────────────┐                                   │
│  │  Proxy ConfigMap │  (SOCKS5/HTTP proxy list)         │
│  └──────────────────┘                                   │
│                                                          │
│  ┌──────────────────┐                                   │
│  │ ASCII Art PVC    │  (git clone, CronJob updates)     │
│  └──────────────────┘                                   │
└──────────────────────────────────────────────────────────┘
```

**Redis usage:**
- **Pub/Sub channels**: `funbot:cmd:<network>` for commands from controller to workers. `funbot:status` for worker to controller status reports.
- **Keys**: `funbot:state:<network>:<pod>` — JSON blob of each pod's state (connected clients, nicks, channels, health). TTL-based expiry for automatic stale detection.
- **No cross-worker coordination needed** — workers only talk to Redis, never to each other. The controller is the single consumer of all status data.

### 2.3 Pod Architecture

**One Deployment per network:**
- Maps directly to `kubectl scale deployment/funbot-worker-efnet --replicas=N`
- Each pod runs multiple IRC clients as goroutines (up to per-IP connection limit)
- Scaling adds pods = new IPs = bypasses per-IP limits
- Network-specific config (server addresses, flood limits) is per-Deployment
- Simpler failure isolation (a pod crash only affects one network)

### 2.4 IRC Client Model (within a single pod)

Each pod runs a `ClientManager` that manages N IRC connections as goroutines:

```
Pod Process
├── ClientManager
│   ├── Client 1 (goroutine) ─── TCP conn to irc.efnet.org
│   ├── Client 2 (goroutine) ─── TCP conn to irc.efnet.org
│   └── Client 3 (goroutine) ─── TCP conn to irc.efnet.org
├── Redis Subscriber (goroutine) ─── listens for commands
├── Status Reporter (goroutine) ─── heartbeat to Redis
└── Proxy Manager ─── assigns proxies to clients
```

## 3. Technology Choices

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.22+ | Goroutine concurrency, small containers, k8s ecosystem |
| IRC Library | `github.com/lrstanley/girc` | Well-maintained, supports IRC color codes, SOCKS proxy |
| Redis Client | `github.com/redis/go-redis/v9` | Official, context-aware, pub/sub support |
| K8s Client | `k8s.io/client-go` | For scaling Deployments from the controller |
| Config | YAML + `github.com/spf13/viper` | File-based defaults + env var overrides |
| Logging | `log/slog` (stdlib) | Structured JSON logging, no external deps |
| Container | Multi-stage Dockerfile, distroless base | Minimal attack surface, small image |
| Proxy | SOCKS5 via `golang.org/x/net/proxy` | Most IRC-compatible proxy type |

## 4. Project Structure

```
funbot/
├── cmd/
│   └── funbot/
│       └── main.go              # Entry point, role selection
├── internal/
│   ├── config/
│   │   └── config.go            # YAML config loading, validation
│   ├── controller/
│   │   ├── controller.go        # Controller main loop
│   │   ├── commands.go          # IRC command parser & dispatcher
│   │   ├── context.go           # Active network/channel context
│   │   ├── status.go            # Status aggregation from Redis
│   │   └── scaler.go            # K8s scaling logic
│   ├── worker/
│   │   ├── worker.go            # Worker main loop
│   │   ├── clientmgr.go         # Manages N IRC clients per pod
│   │   └── executor.go          # Executes commands from controller
│   ├── irc/
│   │   ├── client.go            # Single IRC client wrapper around girc
│   │   ├── flood.go             # Flood control / rate limiting
│   │   └── keepnick.go          # Nick retention logic
│   ├── art/
│   │   ├── repo.go              # Git clone/pull management
│   │   ├── catalog.go           # Art file indexing & search
│   │   └── player.go            # Multi-client coordinated playback
│   ├── proxy/
│   │   └── proxy.go             # Proxy list loading & assignment
│   ├── redis/
│   │   ├── client.go            # Redis connection setup
│   │   ├── pubsub.go            # Pub/sub helpers
│   │   └── state.go             # State read/write helpers
│   └── auth/
│       └── auth.go              # Nick+hostname authorization check
├── config/
│   └── funbot.yaml              # Default configuration file
├── deploy/
│   ├── docker/
│   │   └── Dockerfile           # Multi-stage build
│   ├── docker-compose.yaml      # Local dev environment
│   └── k8s/
│       ├── namespace.yaml
│       ├── redis.yaml            # Redis StatefulSet + Service
│       ├── controller.yaml       # Controller Deployment
│       ├── worker-template.yaml  # Template for network worker Deployments
│       ├── configmap.yaml        # Shared config
│       ├── proxy-secret.yaml     # Proxy list (Secret)
│       ├── art-pvc.yaml          # PersistentVolumeClaim for art repo
│       └── art-cronjob.yaml      # CronJob for git pull
├── go.mod
├── go.sum
├── Makefile                      # Build, test, docker, deploy targets
├── PLAN.md                       # This file
└── AGENTS.md                     # AI agent guidelines
```

## 5. Configuration

### 5.1 Config File (`funbot.yaml`)

```yaml
role: controller  # or "worker", overridden by FUNBOT_ROLE env

controller:
  home_network: "homenet"
  auth:
    nick: "myname"
    hostname: "my.host.mask"

redis:
  address: "redis:6379"
  password: ""
  db: 0

networks:
  homenet:
    servers:
      - "irc.home.net:6697"
    ssl: true
    nick_prefix: "funbot"
    max_clients_per_ip: 3
    channels:
      - "#control"
    flood_delay_ms: 500

  efnet:
    servers:
      - "irc.efnet.org:6667"
    ssl: false
    nick_prefix: "fun"
    max_clients_per_ip: 5
    channels: []
    flood_delay_ms: 1000

proxies:
  file: "/etc/funbot/proxies.txt"  # one per line: socks5://host:port

art:
  repo_url: "https://github.com/birdneststream/asciiart.git"
  local_path: "/data/asciiart"
  update_interval: "1h"

logging:
  level: "info"  # debug, info, warn, error
  format: "json"
```

## 6. Feature Specifications

### 6.1 Command Context

Users can set an active context to avoid repeating network/channel in every command:

| Command | Syntax | Description |
|---------|--------|-------------|
| `!context` | `!context` | Show current active context |
| `!context` | `!context <network>` | Set active network |
| `!context` | `!context <network> <#channel>` | Set active network and channel |
| `!context` | `!context clear` | Clear context |

When a context is set:
- Commands that require a `<network>` argument will use the context network if not explicitly provided
- Commands that require a `<#channel>` argument will use the context channel if not explicitly provided
- Explicitly provided arguments always override the context

### 6.2 Command System

Commands are issued via IRC PM to the controller bot on the home network. The controller parses, validates, and routes them via Redis pub/sub.

**Command prefix**: `!` (configurable)

| Command | Syntax | Description |
|---------|--------|-------------|
| `!status` | `!status [network]` | Show summary of all networks, or detail for one |
| `!networks` | `!networks` | List all connected networks and client counts |
| `!connect` | `!connect <network> <server:port> [ssl]` | Add a new network at runtime |
| `!disconnect` | `!disconnect <network>` | Disconnect all clients from a network |
| `!join` | `!join [network] <#channel> [count]` | Join count clients to a channel (default: 1) |
| `!part` | `!part [network] <#channel> [count\|all]` | Part clients from a channel |
| `!nick` | `!nick [network] <client_id\|all> <newnick>` | Change nick |
| `!keepnick` | `!keepnick [network] <client_id> <desirednick>` | Persistently attempt to acquire nick |
| `!pm` | `!pm [network] <user> <count> <message>` | PM a user using count clients |
| `!say` | `!say [network] [#channel] <count> <message>` | Send message to channel using count clients |
| `!art` | `!art [network] [#channel] [count] <artname>` | Play ASCII art |
| `!artlist` | `!artlist [category]` | List available art files / categories |
| `!artsearch` | `!artsearch <query>` | Search art files by name |
| `!scale` | `!scale <network> <replicas>` | Manually set pod replica count |
| `!proxy` | `!proxy list\|reload` | List loaded proxies or reload from file |
| `!raw` | `!raw [network] <client_id\|all> <raw irc command>` | Send raw IRC command |
| `!context` | `!context [network] [#channel]\|clear` | Set/show/clear active context |

**Note**: `[network]` and `[#channel]` in brackets are optional when a context is set.

**Command routing flow:**
1. User sends PM to controller on home network
2. Controller validates auth (nick+hostname match)
3. Controller parses command, resolving context where arguments are omitted
4. If scaling required, controller estimates need and asks for confirmation
5. Controller publishes command to `funbot:cmd:<network>` Redis channel
6. Worker(s) for that network receive and execute
7. Workers publish result/ack to `funbot:status`
8. Controller relays result back to user via IRC PM

### 6.3 Multi-Client Coordination

When a command specifies `count` clients:
- **Within a pod**: The `ClientManager` selects N available clients and coordinates directly (shared memory, channels)
- **Across pods**: The controller publishes the command; each pod claims a portion of the work via Redis (atomic counter). E.g., "play art, 6 clients needed" -> pod 1 claims clients 1-3, pod 2 claims clients 4-6

### 6.4 ASCII Art Playback

**Art storage**: Git clone to a PersistentVolume, updated via CronJob (`git -C /data/asciiart pull`) every hour. Alternatively, the worker pod itself runs a background goroutine that does `git pull` on the configured interval.

**Art file format**: Plain text files with mIRC color codes (e.g., `\x03fg,bg`). Each line in the file is one IRC message.

**Playback engine** (`internal/art/player.go`):
1. Load art file, parse into lines
2. Determine number of clients needed (provided by user, or auto-detect based on line count and flood delay)
3. Assign lines to clients in round-robin: client 0 gets lines 0, N, 2N...; client 1 gets lines 1, N+1, 2N+1...
4. Coordinated send: a single goroutine acts as conductor, signaling each client to send its next line in sequence, respecting the network's flood delay
5. Each line is sent as-is (preserving mIRC color codes) via `PRIVMSG #channel :<line>`

**Flood control**: Configurable per-network `flood_delay_ms`. The playback engine inserts this delay between consecutive messages from the same client. With N clients, effective inter-line delay = `flood_delay_ms / N`.

### 6.5 Keepnick

`internal/irc/keepnick.go`:
- When activated, starts a goroutine per client that periodically (every 30s, configurable) attempts `NICK <desired>`
- Listens for `QUIT` events on the network -- if the user holding the desired nick quits, immediately attempt the nick change
- Stops when nick is acquired or cancelled

### 6.6 Proxy Support

`internal/proxy/proxy.go`:
- Loads proxy list from file (SOCKS5 format: `socks5://host:port` or `socks5://user:pass@host:port`)
- Maintains a pool of available proxies
- When a new IRC client is created, it can be assigned a proxy from the pool
- Direct connections (no proxy) are used first up to `max_clients_per_ip`, then proxies are used for additional clients
- Each proxy assumed to provide one additional IP, so one additional connection slot
- Proxy health checking: if a connection through a proxy fails, mark it unhealthy and rotate

### 6.7 Scaling

`internal/controller/scaler.go`:
- Uses `client-go` to interact with the Kubernetes API
- When a command requires more clients than currently available on a network:
  1. Calculate: `needed_clients - available_clients = deficit`
  2. Calculate: `pods_needed = ceil(deficit / max_clients_per_ip)`
  3. Also consider available proxies (each proxy adds capacity)
  4. Report to user: "Need X more clients. Scale network Y to Z pods? `!confirm`"
  5. On `!confirm`, execute: `scale deployment/funbot-worker-<network> --replicas=<new_count>`
  6. Wait for pods ready, then re-execute the original command
- When running locally (docker-compose), scaling is not available; the bot reports the limitation

### 6.8 Status Reporting

Workers publish heartbeats to Redis every 10 seconds:

```json
{
  "pod": "funbot-worker-efnet-abc123",
  "network": "efnet",
  "clients": [
    {
      "id": "efnet-0",
      "nick": "fun1",
      "connected": true,
      "channels": ["#test"],
      "proxy": null,
      "keepnick": null
    }
  ],
  "timestamp": "2026-03-25T12:00:00Z"
}
```

The controller aggregates these into a summary for `!status`.

## 7. Authentication

- Config specifies authorized `nick` and `hostname` (from IRC hostmask)
- Every incoming PM is checked: `msg.Source.Name == auth.nick && msg.Source.Host == auth.hostname`
- If mismatch, command is silently ignored (no response to unauthorized users)
- Hardcoded single-user auth per requirement

## 8. Docker & Local Development

### 8.1 Dockerfile (multi-stage)

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /funbot ./cmd/funbot

FROM gcr.io/distroless/static-debian12
COPY --from=builder /funbot /funbot
ENTRYPOINT ["/funbot"]
```

### 8.2 docker-compose.yaml (local dev)

Provides Redis + controller + worker services. Workers can be scaled with `docker compose up --scale worker-testnet=3`.

## 9. Kubernetes Deployment

### 9.1 Resources

| Resource | Name | Notes |
|----------|------|-------|
| Namespace | `funbot` | Isolate all resources |
| StatefulSet | `redis` | Single replica, PVC for persistence |
| Deployment | `funbot-controller` | 1 replica, never scaled |
| Deployment | `funbot-worker-<network>` | 1 per network, scalable |
| ConfigMap | `funbot-config` | `funbot.yaml` |
| Secret | `funbot-proxies` | Proxy list |
| PVC | `funbot-art` | Shared art repo storage |
| CronJob | `funbot-art-update` | `git pull` every hour |
| ServiceAccount | `funbot-controller` | RBAC for scaling Deployments |
| ClusterRole/Binding | | Allow controller to scale worker Deployments |

### 9.2 Runtime Network Addition

When `!connect <network> <server:port>` is issued:
1. Controller creates a new Deployment via the Kubernetes API (using the worker template + network-specific config passed as env vars)
2. The new worker pod starts, connects to the server, and begins reporting status
3. This is ephemeral -- not persisted to config. On controller restart, only default networks from config are reconnected.

### 9.3 RBAC

The controller's ServiceAccount needs:
- `apps/v1 deployments`: `get`, `list`, `create`, `update`, `patch` (in the `funbot` namespace only)
- `v1 pods`: `get`, `list` (for readiness checking)

## 10. Build Phases

### Phase 1: Foundation
- [x] Initialize Go module, project structure, Makefile
- [x] Config loading (YAML + env vars via viper)
- [x] Single IRC client wrapper around `girc` (connect, join, send, receive)
- [x] Auth module (nick+hostname check)
- [x] Command parser with context support
- [x] Dockerfile + docker-compose with Redis
- [x] Basic commands: `!status`, `!nick`, `!join`, `!part`, `!context`
- **Milestone**: Bot connects to one network, responds to authorized commands

### Phase 2: Multi-Client & Redis
- [x] Redis client setup, pub/sub helpers, state read/write
- [x] ClientManager -- manage N clients per pod as goroutines
- [x] Worker role: subscribe to Redis commands, execute, report status
- [x] Controller role: parse IRC commands, publish to Redis, aggregate status
- [x] Status reporting (heartbeat -> Redis -> controller -> IRC)
- **Milestone**: Controller on home net, worker on test net, commands flow end-to-end

### Phase 3: Advanced IRC Features
- [x] Keepnick implementation
- [x] Multi-client PM (`!pm` with count)
- [x] Multi-client channel messaging (`!say`)
- [x] Proxy support (loading, assignment, health checking)
- [x] `!raw` command
- **Milestone**: Full IRC command set working across networks with proxy support

### Phase 4: ASCII Art
- [ ] Git clone/pull management for art repo
- [ ] Art catalog -- indexing files, search by name/category
- [ ] Art parser (handle mIRC color codes, line splitting)
- [ ] Single-client playback with flood control
- [ ] Multi-client coordinated playback
- [ ] `!art`, `!artlist`, `!artsearch` commands
- **Milestone**: Art plays correctly in channels, multi-client coordination works

### Phase 5: Kubernetes & Scaling
- [ ] Kubernetes manifests (all resources listed in 9.1)
- [ ] Controller scaler -- detect capacity shortage, prompt user, scale Deployments
- [ ] Runtime network addition (`!connect` creates Deployment via API)
- [ ] RBAC setup
- [ ] Art CronJob for repo updates
- [ ] Health checks (liveness/readiness probes)
- [ ] Graceful shutdown (SIGTERM handling, clean IRC QUIT)
- **Milestone**: Full system running in Kubernetes, scaling works

### Phase 6: Polish & Hardening
- [ ] Reconnection logic (network drops, server disconnects)
- [ ] Error handling and edge cases
- [ ] Comprehensive structured logging
- [ ] Connection pooling for proxies
- [ ] Rate limiting safety nets (never exceed network flood limits)
- [ ] Integration testing with a local IRC server (e.g., `ergo`)
- **Milestone**: Production-ready

## 11. Testing Strategy

- **Unit tests**: Config parsing, command parsing, auth, art line distribution, flood timing
- **Integration tests**: Use `ergo` (Go-based IRCd) in docker-compose as a test IRC server. Spin up controller + workers, execute commands, verify behavior.
- **Manual testing**: Against real IRC networks during Phase 3+

## 12. Key Design Decisions

| Decision | Choice | Why |
|----------|--------|-----|
| Single binary, two roles | `--role` flag | Simpler CI/CD, one image |
| One Deployment per network | vs. multi-network pods | Clean k8s scaling, failure isolation |
| Redis for coordination | vs. gRPC mesh | Simpler, no service discovery needed, pub/sub is natural fit |
| Workers don't know about each other | Hub-and-spoke via Redis | Simpler worker code, controller aggregates |
| Art on PVC + CronJob | vs. baked into image | Supports frequent repo updates without rebuilds |
| girc library | vs. raw TCP | Handles IRC protocol edge cases, color code support, actively maintained |
| Proxies as SOCKS5 | vs. HTTP CONNECT | SOCKS5 works natively with raw TCP (IRC) |
| Command context | Network/channel defaults | Reduces repetition for the operator |
| No runtime persistence | Default nets only on restart | Simplicity first, can add later |
