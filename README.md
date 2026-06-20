# wally-preview

A small, SSRF-hardened **Matrix URL-preview service**, written in Go.

It sits in front of a Matrix homeserver and answers the standard
`/_matrix/.../preview_url` endpoints itself, so **the homeserver process never
fetches attacker-controlled content**. Because it speaks the unmodified Matrix
client-server API, every Matrix client (Cinny, Element, gomuks, …) gets safe
link previews — there is **no client-side feature to install** and nothing
specific to any one client or server build.

```
                 ┌─────────────────────────────────────────────┐
   Matrix client │                                             │
   ──────────────┤  Caddy / nginx                              │
   GET …/preview_url   │                                       │
                 │     ├── …/preview_url ──▶ wally-preview ─────┼──▶ the open internet
                 │     │                        │  (SSRF-guarded fetch)
                 │     └── everything else ─▶ homeserver        │
                 │                              ▲   │           │
                 │       upload og:image (mint mxc)│   │whoami (authz)
                 │                              └───┴───────────┘
                 └─────────────────────────────────────────────┘
```

## Why

Matrix's `preview_url` makes **your homeserver** fetch arbitrary, user-supplied
URLs. That is a classic SSRF sink: a malicious link can aim the server at
`http://169.254.169.254/` (cloud metadata), internal `10.x` services, etc., and
can abuse it for traffic amplification. Moving the fetch into a **separate,
hardened, isolatable** process keeps the Matrix box out of the blast radius
while still hiding the *client's* IP behind a server-side fetch (the privacy
property you get from server-side previews).

## Security model

The untrusted fetch is hardened to **meet or exceed** a well-configured
homeserver fetcher:

| Protection | wally-preview |
|---|---|
| Internal-IP denylist (RFC1918, loopback, link-local incl. `169.254.169.254`, ULA, CGNAT, multicast, TEST-NETs, reserved, NAT64, IPv4-mapped) | ✅ |
| **DNS-rebinding guard** | ✅ validates the resolved IP **at dial time** (`net.Dialer.Control`) — the IP checked *is* the IP connected to, so there is no check-then-connect gap |
| **Redirect re-validation** | ✅ every redirect hop is a fresh dial, so the IP check re-fires on each one |
| Body size cap | ✅ `io.LimitReader` on both HTML and image bytes |
| Scheme restricted to http/https | ✅ (enforced on redirects too) |
| Timeouts / redirect cap | ✅ dial + TLS + header + overall / max 3 redirects |
| **Not an open proxy** | ✅ requires a valid Matrix access token (verified via `/whoami`) before any fetch |
| Credential isolation | ✅ the outbound client never sends the user's token to a previewed site; `*_proxy` env is ignored |
| Concurrency cap + caching | ✅ bounded in-flight fetches; positive **and** negative per-URL caching |

**The dial-time IP check is the linchpin.** Validating a hostname's IP *before*
issuing the request (resolve → check → fetch) is unsafe — DNS can return a
different answer on the real connection (DNS rebinding), and redirects bypass it
entirely. This service validates the IP the socket is actually connecting to, on
every connection, which closes both holes.

### Operational hardening (recommended)

The in-process guard is the primary defense; isolate the process too:

- **Bind to localhost** (default) — only your reverse proxy can reach it.
- **Firewall egress to public IPs.** Run it in a container/namespace whose
  outbound traffic cannot reach your LAN, so even a bug in the guard can't
  pivot inward. Example (nftables, drop private destinations from the unit's
  user/cgroup) is left to your environment.
- **Use a dedicated, low-privilege Matrix account** for `WALLY_PREVIEW_UPLOAD_TOKEN`.
  It only needs media upload. If the token leaks, the blast radius is that one
  account's media.

## Configuration

All via environment variables (see [`.env.example`](.env.example)):

| Variable | Default | Purpose |
|---|---|---|
| `WALLY_PREVIEW_LISTEN` | `127.0.0.1:8088` | listen address (keep on localhost) |
| `WALLY_PREVIEW_HOMESERVER` | `http://127.0.0.1:6167` | local HS for whoami + upload |
| `WALLY_PREVIEW_UPLOAD_TOKEN` | *(required)* | dedicated low-priv account token |
| `WALLY_PREVIEW_ALLOW_DOMAINS` | *(empty = any public host)* | optional comma-separated allowlist |
| `WALLY_PREVIEW_MAX_HTML_BYTES` | `262144` | HTML read cap |
| `WALLY_PREVIEW_MAX_IMAGE_BYTES` | `5242880` | image read cap |
| `WALLY_PREVIEW_CACHE_TTL` | `1h` | positive cache TTL |
| `WALLY_PREVIEW_NEGATIVE_TTL` | `5m` | failed-URL cache TTL |
| `WALLY_PREVIEW_MAX_CONCURRENCY` | `8` | in-flight fetch cap |
| `WALLY_PREVIEW_REQUEST_TIMEOUT` | `10s` | per-fetch overall timeout |

## Deploy

### Automated Setup (Recommended)

You can use the interactive setup script to compile, install the binary and systemd service, auto-generate credentials, retrieve the access token, and configure the environment:

```bash
./setup.sh
```

### Manual Setup

1. **Build**: `go build -o wally-preview .` (or use the [`Dockerfile`](Dockerfile)).
2. **Configure**: copy `.env.example` → `/etc/wally-preview.env`, set
   `WALLY_PREVIEW_UPLOAD_TOKEN` (create a dedicated account, log in, grab its
   token).
3. **Run**: install [`wally-preview.service`](wally-preview.service) and the
   binary at `/usr/local/bin/wally-preview`, then `systemctl enable --now
   wally-preview`.
4. **Route**: add the [`Caddyfile.example`](Caddyfile.example) block so the
   `preview_url` paths go to the shim and everything else to the homeserver.
5. **Disable the homeserver's own previews** so the HS process never performs
   the untrusted fetch (e.g. on Continuwuity, leave the URL-preview domain
   allowlist empty). The shim now owns previews for every client.

Verify: `curl 'http://127.0.0.1:8088/_matrix/media/v3/preview_url?url=https://example.com' -H 'Authorization: Bearer <token>'`.

## Endpoints

- `GET /_matrix/media/v3/preview_url`
- `GET /_matrix/media/r0/preview_url`
- `GET /_matrix/client/v1/media/preview_url`
- `GET /healthz`

## License

MIT — see [LICENSE](LICENSE).
