# mole 🦫

A tiny self-hosted tunnel tool: expose a service from a machine **without a
public IP** by dialing *out* to a relay that has one. The same core idea as
Cloudflare Tunnel / ngrok, small enough to read every line of.

```
visitor ──► moled (VPS, public IP) ◄── outbound conns ── mole (laptop) ──► localhost:8000
```

## Components

| Binary   | Runs on            | Role                                                          |
|----------|--------------------|---------------------------------------------------------------|
| `moled`  | VPS / public host  | Speaks HTTP to visitors; routes each Host to its named tunnel |
| `mole`   | laptop / homelab   | Dials out, authenticates under a name, serves streams         |

## Quick start

Everything on one machine first:

```sh
python3 -m http.server 8000 &                          # any local service
go run ./cmd/moled --auth-token sekrit --domain test & # relay
go run ./cmd/mole --token sekrit --name me             # client
curl -H 'Host: me.test' http://localhost:8080          # through the tunnel
```

For real use, run `moled` on a VPS with a public IP and point a wildcard DNS
record at it (`*.example.com A YOUR_VPS_IP`, proxied through Cloudflare if you like):

```sh
GOOS=linux GOARCH=amd64 go build -o /tmp/moled ./cmd/moled
scp /tmp/moled vps:
ssh vps 'sudo ufw allow 7000/tcp,8080/tcp && nohup ./moled \
  --auth-token=LONG_RANDOM --domain=example.com > moled.log 2>&1 &'
go run ./cmd/mole --relay=YOUR_VPS_IP:7000 --token=LONG_RANDOM --name me
# http://me.example.com is now served by your laptop, from anywhere
```

## Flags

**moled**

- `--public-addr` — listen address for visitors (default `:8080`)
- `--tunnel-addr` — listen address for tunnel clients (default `:7000`)
- `--auth-token` — shared secret clients must present (**required**)
- `--domain` — root domain for `<name>.<domain>` routing; empty = exact-Host mode
- `--tunnel-cert`, `--tunnel-key` — TLS for the tunnel port
- `--public-cert`, `--public-key` — HTTPS for the public port

**mole**

- `--relay` — address of `moled`'s tunnel port (default `localhost:7000`)
- `--local` — local service to expose (default `localhost:8000`)
- `--token` — auth token expected by `moled` (**required**)
- `--name` — tunnel name (default: sanitized hostname)
- `--retry-wait` — base pause between relay dial attempts (default `2s`,
  doubles with jitter up to 30s, resets on successful auth)
- `--tls` — dial the relay over TLS
- `--ca` — CA bundle (PEM) to verify the relay's cert; empty = system roots
- `--tls-name` — server name override when verifying the relay's cert

## How it works

1. **Dial out.** The machine behind NAT makes ordinary *outbound* TCP
   connections to the relay. Firewalls allow this by default; replies to an
   established connection may flow both ways. That is the whole trick — the
   "inbound" direction rides inside connections the private side created.
2. **Authenticate & register.** The first frame on a tunnel connection is
   `Auth{name, token}`; the relay answers `AuthAck{domain}` and maps
   `<name>` (or `<name>.<domain>`) to that connection.
3. **Multiplex.** One tunnel connection carries every visitor concurrently.
   `internal/wire` frames each virtual stream as
   `[type:1][streamID:8][length:4][payload]` — the same idea as HTTP/2 or
   QUIC streams, at toy scale.
4. **Route HTTP.** Visitors hit the relay's public port speaking real HTTP.
   The relay picks the tunnel by Host header and reverse-proxies each request
   over a fresh stream (`httputil.ReverseProxy` with a stream-dialing
   Transport). Unknown hosts get a 404 page; dead origins get a 502.

## Security model

- **Tunnel leg** (`mole` ↔ `moled`): enable TLS with `--tunnel-cert/--tunnel-key`
  on the relay and `--tls` (+ `--ca ca.pem` for a private CA) on the client.
  Without it, traffic is plaintext — fine for localhost testing only.
- **Public leg** (visitor ↔ `moled`): serve HTTPS directly with
  `--public-cert/--public-key`, or front `moled` with Caddy/nginx doing ACME.
- **Authentication**: every tunnel connection must present the shared token
  as its first frame; bad tokens are rejected before any traffic flows.
- Tokens are shared secrets; per-client tokens and rate limits are future work.

## Status / known limitations (by design)

- Named tunnels, Host routing, auth tokens, keepalives + auto-reconnect, TLS: done
- Per-stream receive buffers are unbounded (no windowed flow control yet);
  a stalled consumer can grow memory
- Streams ignore deadlines; a wedged origin ties up one goroutine per request

## Roadmap

- [x] Frame-based multiplexing: many visitors share one tunnel connection
- [x] Named tunnels + Host-based routing (`me.example.com` → my laptop)
- [x] Auth tokens so only your client can park tunnels
- [x] Keepalives + automatic reconnection with exponential backoff
- [x] TLS on tunnel and public ports
