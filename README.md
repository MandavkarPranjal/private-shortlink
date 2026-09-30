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
admin override, and JSON-lines backup export.

## Build

Everything goes through [mise](https://mise.jdx.dev): the Go toolchain and the
linters are pinned in `.mise.toml`, and `CGO_ENABLED=0` is set for you.

```sh
mise trust          # once, after cloning: allow .mise.toml
mise install        # install the pinned Go/golangci-lint/staticcheck
mise run build      # -> ./shortlink
```

No cgo, no system dependencies (the SQLite driver is pure Go). `mise run` alone
lists every task; `mise tasks info <name>` describes one.

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
-version            print version and exit
```

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
