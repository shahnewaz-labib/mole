# mole 🦫

A tiny self-hosted tunnel tool: expose a service from a machine **without a
public IP** by dialing *out* to a relay that has one. The same core idea as
Cloudflare Tunnel / ngrok, small enough to read every line of.

```
visitor ──► moled (VPS, public IP) ◄── outbound conns ── mole (laptop) ──► localhost:8000
```

## Components

| Binary   | Runs on            | Role                                                        |
|----------|--------------------|-------------------------------------------------------------|
| `moled`  | VPS / public host  | Accepts visitors and parked tunnel connections; splices them |
| `mole`   | laptop / homelab   | Dials out to `moled`, keeps spare connections parked         |

## Quick start

Everything on one machine first:

```sh
python3 -m http.server 8000 &          # any local service
go run ./cmd/moled &                   # relay: :8080 public, :7000 tunnels
go run ./cmd/mole                      # client: dials out, parks 8 conns
curl http://localhost:8080             # you're talking through the tunnel
```

For real use, run `moled` on a VPS with a public IP:

```sh
GOOS=linux GOARCH=amd64 go build -o /tmp/moled ./cmd/moled
scp /tmp/moled vps:
ssh vps 'sudo ufw allow 7000/tcp,8080/tcp && nohup ./moled > moled.log 2>&1 &'
go run ./cmd/mole --relay=YOUR_VPS_IP:7000
# open http://YOUR_VPS_IP:8080 from anywhere
```

## Flags

**moled**

- `--public-addr` — listen address for visitors (default `:8080`)
- `--tunnel-addr` — listen address for tunnel clients (default `:7000`)

**mole**

- `--relay` — address of `moled`'s tunnel port (default `localhost:7000`)
- `--local` — local service to expose (default `localhost:8000`)
- `--retry-wait` — pause between relay dial attempts (default `2s`)

## How it works

1. **Dial out.** The machine behind NAT makes ordinary *outbound* TCP
   connections to the relay. Firewalls allow this by default; replies to an
   established connection may flow both ways. That is the whole trick — the
   "inbound" direction rides inside connections the private side created.
2. **Multiplex.** One tunnel connection carries every visitor concurrently.
   `internal/wire` frames each virtual stream as
   `[type:1][streamID:8][length:4][payload]` — the same idea as HTTP/2 or
   QUIC streams, at toy scale.
3. **Splice.** When a visitor connects to the public port, the relay opens a
   stream and copies bytes between them verbatim (`io.Copy`, one goroutine
   per direction). Neither side understands HTTP; everything is just bytes on
   streams.

## Status / known limitations (by design)

- Multiplexing: done — one tunnel connection serves unlimited concurrent visitors
- Plaintext everywhere; no authentication (anyone who can reach port 7000 can
  park a tunnel — don't expose it publicly yet)
- No hostname routing: whatever hits the public port goes to your one service
- Per-stream receive buffers are unbounded (no windowed flow control yet);
  a stalled consumer can grow memory

## Roadmap

- [x] Frame-based multiplexing: many visitors share one tunnel connection
- [ ] Named tunnels + Host-based routing (`me.example.com` → my laptop)
- [ ] Auth tokens so only your client can park tunnels
- [ ] Keepalives + automatic reconnection with exponential backoff
- [ ] TLS on tunnel and public ports
