# shortlink

A private, [golink](https://github.com/tailscale/golink)-style URL shortener
for your tailnet. Links live in SQLite, are owned by whoever created them, and
are reachable only from networks you expose it to:

| Mode      | Transport                              | Who can reach it            |
|-----------|----------------------------------------|-----------------------------|
| `tailnet` | Embedded Tailscale node (tsnet, port 80) | Your tailnet only (default) |
| `tailcat` | [tailcat](https://github.com/tailscale/tailcat) `tc…` address | Anyone you share the address with |
| `netbird` | Plain TCP on your [NetBird](https://netbird.io) interface | Your NetBird network only |
| `local`   | Plain TCP listener                     | Local/dev use               |

Features: redirects with click counts, a server-rendered web UI (browse,
search, create, edit, delete), a JSON API, owner-based permissions with
admin override, JSON-lines backup export, structured logs, and systemd/Docker
deployment files.

## Build

Everything goes through [mise](https://mise.jdx.dev): the Go toolchain and the
linters are pinned in `.mise.toml`, and `CGO_ENABLED=0` is set for you.

```sh
mise trust          # once, after cloning: allow .mise.toml
mise install        # install the pinned Go/golangci-lint/staticcheck
mise run build      # -> ./shortlink
```

No cgo, no system dependencies (the SQLite driver is pure Go). `mise run` alone
lists every task; `mise tasks info <name>` describes one. The binary reports the
git revision it was built from, so a deployed build is traceable:

```sh
# a `mise run build` binary, stamped with `git describe`:
./shortlink -version          # shortlink v0.2.1-4-ga1b2c3d
# a bare `go build`, unstamped, plus the commit it came from:
#                              # shortlink 0.2.0+a1b2c3d   (…-dirty if uncommitted)
```

Secrets you don't want in your shell history can live in a `.env` file (mise
loads it automatically, and it is gitignored):

```sh
TS_AUTHKEY=tskey-auth-...
NETBIRD_API_TOKEN=nbp_...
```

## Run

### tailnet mode (default)

Runs an embedded Tailscale node that serves HTTP on port 80 of your tailnet.
The node advertises the MagicDNS name `go` by default, so links look like
`http://go/whatever` — the same ergonomics as golink.

```sh
export TS_AUTHKEY=tskey-auth-...   # only needed on first login
./shortlink                        # -mode tailnet is the default
```

First run prints an auth URL if no key is set. State persists under
`~/.config/shortlink-tsnet/` (override with `-state-dir`), so the auth key is
only needed once. Use `-hostname mylinks` to advertise a different name.

Requests are identified via tailscaled's WhoIs API: the owner of a link is the
caller's tailnet login name (e.g. `alice@example.com`).

**Admin grants.** Only the owner of a link may edit or delete it, unless the
caller holds an admin grant. Add it to your tailnet ACLs:

```jsonc
"grants": [
  {
    "src":    ["group:admins"],
    "dst":    ["*"],
    "ip":     ["80"],
    "app": {
      "tailscale.com/cap/shortlink": [{"admin": true}]
    }
  },
]
```

### tailcat mode

tailcat is the Tailscale data plane (WireGuard + magicsock + DERP) without a
control plane: no tailnet, no accounts, no coordination server. The server
generates a `tc…` address (stored, with its keys, in `-key-file`) that you
share out of band with the people who may use the service.

```sh
./shortlink -mode tailcat
```

On startup it prints:

```
Tailcat address (share this with your clients):
  tcABC...xyz
Clients can then run:  tailcat open tcABC...xyz
Or forward a local port:  tailcat forward tcABC...xyz 18080:80
Node key file: shortlink-tailcat.key (keep it to keep the address stable)
```

Clients need the [tailcat CLI](https://github.com/tailscale/tailcat):

```sh
tailcat open tcABC...xyz          # opens http://127.0.0.1:<random>/
tailcat forward tcABC...xyz 18080:80   # or pick your own local port
```

Because there is no control plane, callers cannot be resolved to tailnet
identities: each client is identified by its stable synthetic tailcat address
(the host part of its remote address). Ownership is recorded per address.

**Keep `-key-file` secret and backed up** — it is what makes the `tc…`
address stable across restarts (both the node key and the preshared key are
required).

### netbird mode

Serves on your NetBird network: the server binds this host's NetBird WireGuard
interface (default `wt0`, port 80) with a plain TCP listener, so it is only
reachable through the mesh, and resolves callers through the NetBird
management API.

```sh
export NETBIRD_API_TOKEN=nbp_...    # dashboard: Users → Me → API tokens
./shortlink -mode netbird
```

The token only needs read access to peers and users. Self-hosted NetBird?
Point `-netbird-api` at your management server (e.g.
`http://localhost:33071`). If the WireGuard interface has another name
(`netbird0`, `utun…`), set `-netbird-iface`. Binding port 80 needs root or
`CAP_NET_BIND_SERVICE` (`sudo setcap cap_net_bind_service=+ep shortlink` after
each build); otherwise run `-listen <netbird-ip>:8080` — keep the mesh IP in
the address, a bare `:8080` would also expose the service on your LAN.

On startup it prints something like:

```
Shortlink is on your NetBird network at http://go.netbird.cloud/
  Identity: caller's NetBird user email via https://api.netbird.io
  Bound to 100.64.0.1:80 on interface "wt0" (-listen to override)
  Admins: peers in the "shortlink-admin" NetBird group
```

Requests are identified by the caller's NetBird **user email**
(e.g. `alice@example.com`), so a person owns their links no matter which
device they use. The peer/IP directory is refreshed from the API every
minute; callers unknown to it fall back to their raw mesh IP.

**Admin grants.** Peers in the `-netbird-admin-group` group (default
`shortlink-admin`, exact name match, empty means nobody) are app admins.
Create the group in the NetBird dashboard and put your admin peers in it.

### local mode

Plain HTTP on localhost, for development or when something else fronts it:

```sh
./shortlink -mode local -listen 127.0.0.1:8080
```

Identity is `local` with admin rights; use `-open` in other modes to record
ownership but skip permission checks.

## Web UI

Open `/` in a browser:

- create a link with the name + URL form (multi-segment names like `docs/go`
  work),
- search by name or URL (`/?q=...`),
- edit/delete via per-row buttons (owner or admin only),
- click counts and last-updated timestamps per link.

Names: lowercase letters, digits, `-`, `.`, `_`, separated by `/`; URLs must be
absolute `http(s)://` values. The `/-/` prefix is reserved for management
routes, so link names may not start with `-`.

## JSON API

| Method | Path              | Description                          |
|--------|-------------------|--------------------------------------|
| GET    | `/-/api`          | List links (`?q=` filter)            |
| GET    | `/-/api/{name}`   | One link, 404 if missing             |
| POST   | `/-/save`         | Create/update (owner or admin)       |
| POST   | `/-/delete`       | Delete (owner or admin)              |
| GET    | `/-/export`       | Backup: one JSON link per line       |

`POST /-/save` and `POST /-/delete` accept either form fields or JSON
(`Content-Type: application/json`), and answer JSON when sent JSON:

```sh
curl -X POST http://go/-/save \
  -H 'Content-Type: application/json' \
  -d '{"name":"blog","url":"https://example.com"}'

curl http://go/-/api/blog
# {"ok":true,"link":{"name":"blog","url":"https://example.com",
#   "owner":"alice@example.com","clicks":0,...}}
```

## Flags

```
-mode string        tailnet, tailcat, netbird, or local (default "tailnet")
-listen string      listen address (default ":80" tailnet/tailcat/netbird, ":8080" local)
-db string          SQLite database path (default "shortlink.db")
-hostname string    tailnet: MagicDNS hostname to advertise (default "go")
-ts-authkey string  tailnet: auth key (default $TS_AUTHKEY; unused after first login)
-state-dir string   tailnet: tsnet state directory (default under user config dir)
-key-file string    tailcat: persistent node key file (default "shortlink-tailcat.key")
-netbird-api string netbird: management API base URL (default "https://api.netbird.io")
-netbird-token string  netbird: management API token (default $NETBIRD_API_TOKEN)
-netbird-iface string  netbird: WireGuard interface to bind (default "wt0")
-netbird-admin-group string  netbird: peer group granted admin (default "shortlink-admin")
-open               disable ownership checks (ownership still recorded)
-log-level string   log level: debug, info, warn, error (default "info")
-log-format string  log format: text or json (default "text")
-version            print version and exit
```

Every flag also reads a `SHORTLINK_<FLAG>` environment variable, so a systemd
`EnvironmentFile`, a `.env` file or a container environment can configure the
service without a long command line: `-log-level` reads `SHORTLINK_LOG_LEVEL`,
`-db` reads `SHORTLINK_DB`, `-netbird-api` reads `SHORTLINK_NETBIRD_API`. A
flag given on the command line always wins. `TS_AUTHKEY` and
`NETBIRD_API_TOKEN` keep their existing names.

A variable that is set but empty counts as a value, which is how a setting that
defaults to something gets turned off:

```sh
SHORTLINK_NETBIRD_ADMIN_GROUP=    # no netbird peer gets admin rights
```

An unusable value is fatal only if no flag overrode it, so
`SHORTLINK_LOG_FORMAT=xml ./shortlink -log-format json` starts normally.

## Logging

Logs are structured (`log/slog`) and go to stderr, ready for the journal or a
shipper:

```sh
./shortlink -mode local -log-format json -log-level debug
```

One line per request:

```
level=WARN msg=request method=GET path=/-/edit/blog status=403 bytes=29 duration=142µs who=bob
```

`who` is the identity the route resolved — a tailnet login name, a tailcat
address, or `anonymous` when there is none — and the peer's address when the
route needed no identity at all. The lookup is deferred until a handler asks for
it, because in tailnet mode it is a WhoIs round-trip to tailscaled and redirects
are the hot path:

```
level=DEBUG msg=request method=GET path=/blog status=302 bytes=47 duration=142µs who=100.64.0.7
```

Levels are chosen so that `-log-level info` shows only what needs attention:
4xx is `WARN`, 5xx is `ERROR`, and successful requests and redirects are
`DEBUG` — every click on a link is a redirect, so logging those at info would
bury the rest. A panicking handler still gets its access line, as a 500.
`debug` also surfaces the tailcat backend chatter; the tsnet login URL on first
run is logged at `INFO`, because it is the one line you cannot do without.

## Deployment

### systemd

`deploy/shortlink.service` runs the binary as an unprivileged `shortlink` user
with `StateDirectory=shortlink`, a locked-down capability set, and the standard
hardening directives. It passes no `-mode`, so `SHORTLINK_MODE` in the
environment file is the single place to choose the mode.

No mode needs a capability except for one case: binding a port below 1024 needs
`CAP_NET_BIND_SERVICE`. Tailnet mode needs none at all — tsnet embeds a
userspace network stack (gVisor), so it creates no TUN device and needs no
elevated rights. NetBird mode binds a port on the host's existing WireGuard
interface and does not manage the device either.

```sh
# build and install
mise run build
sudo useradd --system --no-create-home --shell /usr/sbin/nologin shortlink
sudo install -m 0755 shortlink /usr/local/bin/shortlink

# configuration (secrets live here)
sudo install -d -m 0750 -o shortlink -g shortlink /etc/shortlink
sudo install -m 0600 -o shortlink -g shortlink \
  deploy/shortlink.env.example /etc/shortlink/shortlink.env
sudo $EDITOR /etc/shortlink/shortlink.env

sudo install -m 0644 deploy/shortlink.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now shortlink
journalctl -u shortlink -f
```

### Docker

The image is multi-stage and ends on `distroless/static`: a fully static
binary, no shell, no package manager. It runs as uid 65532 and keeps state in
two volumes, `/data` (the database) and `/config` (tsnet state, tailcat key):

```sh
docker build -t shortlink:local .
docker run -d --name shortlink \
  -p 8080:8080 \
  -v shortlink-data:/data -v shortlink-config:/config \
  shortlink:local
```

The default command is local mode on port 8080. Overriding it replaces the CMD
wholesale, so a mesh-mode run has to repeat the paths it wants persistent —
`-db` and `-key-file` both name files inside the volumes, and neither is a
default that points there. Neither mesh mode needs a TUN device: tsnet's stack
is userspace, tailcat is WireGuard plus DERP with no control plane, and netbird
binds a port on the host's existing WireGuard interface.

```sh
# tailnet: the embedded node serves on the tailnet, so it needs no port
# published and no capabilities at all
docker run -d --name shortlink \
  -e TS_AUTHKEY=tskey-auth-... \
  -v shortlink-data:/data -v shortlink-config:/config \
  shortlink:local -mode tailnet \
  -db /data/shortlink.db -state-dir /config/shortlink-tsnet

# tailcat: a tc… address, reached through the tailcat CLI on the client
docker run -d --name shortlink \
  -v shortlink-data:/data -v shortlink-config:/config \
  shortlink:local -mode tailcat \
  -db /data/shortlink.db -key-file /config/shortlink-tailcat.key

# netbird: binds port 80 on the host's mesh interface, which needs the host
# network namespace and NET_BIND_SERVICE
docker run -d --name shortlink \
  --network host --cap-add NET_BIND_SERVICE \
  -e NETBIRD_API_TOKEN=nbp_... \
  -v shortlink-data:/data \
  shortlink:local -mode netbird -db /data/shortlink.db
```

Every flag that names a file needs an explicit path here, because the volume
paths are not the binary's defaults. `-key-file` matters as much as `-db`: drop
it and the container writes its node key to a fresh file on every start, so the
tc… address changes and every address you handed out stops resolving.

`mise run docker:build` stamps the version and commit from git. A bare
`docker build` cannot: `.git` is excluded from the build context on purpose, so
pass the version yourself with
`--build-arg VERSION="$(git describe --tags --always --long)"`. Without it the
binary reports itself as an unstamped development build.

## Data

SQLite (WAL mode) in `-db`, table `links(name, url, owner, created_at,
updated_at, clicks)`. Back up with `/-/export` (JSON lines) or copy the
database file while stopped.

## Development

```sh
mise run fmt        # gofmt in place
mise run lint       # staticcheck + golangci-lint
mise run vet        # go vet
mise run test       # unit + handler tests; tailcat e2e relays through DERP
mise run test:short # skip the network e2e test
mise run ci         # fmt:check, vet, lint, test
mise run serve      # local mode on http://127.0.0.1:8080/
mise run run -- -mode local -listen 127.0.0.1:8080   # flags pass through
```

Plain `go build` / `go test` still work if Go is on your PATH; mise just pins the
versions so a teammate and CI get the same toolchain.