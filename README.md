# Funbot

An IRC bot that manages multiple client connections across networks using SOCKS5 proxies. Proxies are sourced from a [proxy-scanner](https://github.com/venatiodecorus/proxy-scanner) API. Commands are issued via private message from an authorized user on a home IRC network.

## Quick Start

### Binary

```bash
make build
./bin/funbot -config config/funbot.yaml
```

### Docker

```bash
mkdir funbot && cd funbot
# Copy docker-compose.yaml from deploy/ and create your funbot.yaml
docker compose up -d
```

See [Deployment](#deployment) for details.

## Configuration

Funbot reads config from YAML with environment variable overrides (prefix `FUNBOT_`, dots become underscores).

Config file search order (when `-config` is not specified):

1. `/etc/funbot/funbot.yaml`
2. `./config/funbot.yaml`
3. `./funbot.yaml`

### Example

```yaml
home_network: "homenet"
command_prefix: "!"

auth:
  nick: "myname"
  hostname: "my.host.mask"

networks:
  homenet:
    servers:
      - "irc.home.net:6697"
    ssl: true
    nick_prefix: "funbot"
    channels:
      - "#control"
    flood_delay_ms: 500
  efnet:
    servers:
      - "irc.efnet.org:6667"
    nick_prefix: "fun"
    channels:
      - "#test"
    flood_delay_ms: 500
    default_clients: 3

proxies:
  api_url: "http://localhost:8080"
  protocol: "socks5"
  max_latency: 1000
  refresh_interval: "5m"

art:
  repo_url: "https://github.com/birdneststream/asciiart.git"
  local_path: "/data/asciiart"
  update_interval: "1h"

logging:
  level: "info"
  format: "json"
```

### Reference

#### Top-level

| Key | Type | Required | Default | Description |
|-----|------|----------|---------|-------------|
| `home_network` | string | yes | | Name of the network used for receiving commands |
| `command_prefix` | string | no | `!` | Prefix for all commands |

#### `auth`

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `nick` | string | yes | Authorized user's IRC nick |
| `hostname` | string | yes | Authorized user's IRC hostname |

Only messages from a user matching both nick and hostname are accepted as commands.

#### `networks.<name>`

| Key | Type | Required | Default | Description |
|-----|------|----------|---------|-------------|
| `servers` | list | yes | | Server addresses (`host:port`) |
| `ssl` | bool | no | `false` | Use TLS |
| `nick_prefix` | string | yes | | Prefix for client nicks (produces `prefix0`, `prefix1`, ...) |
| `channels` | list | no | | Channels to auto-join on connect |
| `flood_delay_ms` | int | no | `500` | Delay between messages (ms) |
| `default_clients` | int | no | `0` | Clients to connect on startup (0 = none) |

The home network uses `nick_prefix` as-is for the single control client. Other networks append an index.

#### `proxies`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_url` | string | | Base URL of the proxy-scanner API |
| `protocol` | string | | Protocol filter (`socks5`, `socks4`, `http`) |
| `max_latency` | int | `0` | Max proxy latency in ms (0 = no filter) |
| `refresh_interval` | string | `5m` | How often to re-fetch from the API |

Proxies are fetched from `GET <api_url>/v1/proxies` with the configured filters. The pool is refreshed on the configured interval. In-use proxies are preserved across refreshes.

#### `art`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `repo_url` | string | `https://github.com/birdneststream/asciiart.git` | Git repo containing ASCII art files |
| `local_path` | string | `/data/asciiart` | Local path for the cloned repo |
| `update_interval` | string | `1h` | How often to pull updates |

#### `logging`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `level` | string | `info` | `debug`, `info`, `warn`, or `error` |
| `format` | string | `json` | `json` or `text` |

## Commands

All commands are sent as private messages to the bot on the home network. Only the authorized user (matching `auth.nick` and `auth.hostname`) can issue commands.

### Context System

Many commands accept optional `[network]` and `[#channel]` arguments. If omitted, the bot falls back to the active context. Set context with `!context` to avoid repeating network/channel on every command.

```
!context efnet #test     -- set both network and channel
!context efnet           -- set network only
!context clear           -- clear context
!context                 -- show current context
```

Explicit arguments always override context.

### Network Management

| Command | Description |
|---------|-------------|
| `!connect <network> <server:port> [nick_prefix] [count] [ssl]` | Connect to a new network with proxy-backed clients |
| `!disconnect <network>` | Disconnect from a network |
| `!addclients <network> <count>` | Add more clients to a connected network |
| `!rmclients <network> <count>` | Remove clients from a network |
| `!networks` | List all configured and runtime networks |

`!connect` creates a runtime-only network that will not survive a restart. Default nick prefix is `fun`, default client count is `1`. Add `ssl` to enable TLS.

```
!connect efnet irc.efnet.org:6667 fun 5
!connect dalnet irc.dal.net:6697 bot 3 ssl
!addclients efnet 10
!rmclients efnet 5
!disconnect efnet
```

### Channel Operations

| Command | Description |
|---------|-------------|
| `!join [network] <#channel> [count]` | Join clients to a channel |
| `!part [network] <#channel> [count\|all]` | Part clients from a channel |

If `count` is omitted or `all`, all connected clients are used.

```
!join efnet #test 5      -- join 5 clients to #test on efnet
!join #test              -- join all clients (uses context network)
!part efnet #test all
```

### Messaging

| Command | Description |
|---------|-------------|
| `!say [network] [#channel] <count> <message>` | Send a message to a channel |
| `!pm [network] <user> <count> <message>` | Send a private message to a user |

```
!say efnet #test 3 hello world    -- 3 clients say "hello world" in #test
!pm efnet someuser 1 hey there    -- 1 client PMs someuser
```

### Nick Management

| Command | Description |
|---------|-------------|
| `!nick [network] <client_id\|all> <newnick>` | Change client nick(s) |
| `!keepnick [network] <client_id> <desirednick\|stop>` | Continuously try to acquire a nick |

When using `!nick` with `all`, each client gets `<newnick>0`, `<newnick>1`, etc.

```
!nick efnet efnet-0 coolguy       -- change one client's nick
!nick efnet all bot               -- all clients become bot0, bot1, ...
!keepnick efnet efnet-0 target    -- keep trying to get "target"
!keepnick efnet efnet-0 stop      -- stop trying
```

### ASCII Art

| Command | Description |
|---------|-------------|
| `!art [network] [#channel] [count] <artname>` | Play ASCII art in a channel |
| `!artlist [category]` | List art categories or files in a category |
| `!artsearch <query>` | Search art files by name |

Art is played line-by-line across selected clients with flood delay.

```
!artsearch dragon
!artlist animals
!art efnet #test 5 dragon
```

### Status & Info

| Command | Description |
|---------|-------------|
| `!status [network]` | Show bot status (or detailed network status) |
| `!help` | List available commands |

### Raw IRC

| Command | Description |
|---------|-------------|
| `!raw [network] <client_id\|all> <raw command>` | Send a raw IRC command |

```
!raw efnet all PRIVMSG #test :hello
!raw efnet efnet-0 WHOIS someuser
```

## Deployment

### Docker Compose (recommended)

1. Create a directory with your config:

```bash
mkdir funbot && cd funbot
```

2. Create `docker-compose.yaml`:

```yaml
services:
  funbot:
    image: ghcr.io/venatiodecorus/funbot:latest
    restart: unless-stopped
    volumes:
      - ./funbot.yaml:/etc/funbot/funbot.yaml:ro
      - art-data:/data/asciiart

volumes:
  art-data:
```

3. Create `funbot.yaml` with your settings (see [Configuration](#configuration)).

4. Run:

```bash
docker compose up -d
docker compose logs -f    # watch logs
```

### Building from Source

```bash
git clone https://github.com/venatiodecorus/funbot.git
cd funbot
make build
./bin/funbot -config config/funbot.yaml
```

### Docker Build

```bash
make docker
# or
docker build -f deploy/docker/Dockerfile -t funbot:latest .
```

### Releasing

Tag a version to trigger the GitHub Actions release workflow, which builds and pushes to GHCR:

```bash
git tag v0.1.0
git push origin v0.1.0
```

This produces `ghcr.io/venatiodecorus/funbot:0.1.0`, `:0.1`, and `:sha-<commit>`.

## Development

```bash
make test              # run unit tests with race detector
make lint              # run golangci-lint
make fmt               # format code
make test-integration  # run integration tests (requires Docker)
make deps              # tidy go.mod
```

### Flags

```
-config string    Path to config file
-version          Print version and exit
```

## Architecture

Single Go binary, single process. No Redis, no Kubernetes.

- **Home client**: Connects to the home IRC network to receive commands via PM from the authorized user.
- **Network managers**: Each managed network gets a `NetworkManager` that handles N proxy-backed IRC client connections as goroutines.
- **Proxy pool**: Fetches SOCKS5 proxies from a [proxy-scanner](https://github.com/venatiodecorus/proxy-scanner) API. A single proxy can serve connections to different networks simultaneously. The pool is refreshed periodically.
- **Art system**: Clones a git repo of ASCII art, indexes it, and plays art files line-by-line across clients with flood control.

```
funbot process
  |
  +-- home IRC client (receives commands via PM)
  |
  +-- proxy pool (fetches from proxy-scanner API)
  |
  +-- network manager: efnet
  |     +-- client efnet-0 (via proxy A)
  |     +-- client efnet-1 (via proxy B)
  |     +-- client efnet-2 (via proxy C)
  |
  +-- network manager: undernet
  |     +-- client undernet-0 (via proxy A)  # same proxy, different network
  |     +-- client undernet-1 (via proxy D)
  |
  +-- art repo + catalog
```
