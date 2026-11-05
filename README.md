# algod.top

[![CI](https://github.com/txngroup/algodtop/actions/workflows/ci.yml/badge.svg)](https://github.com/txngroup/algodtop/actions/workflows/ci.yml)

algod.top monitors one or more Algorand nodes (algod). One binary holds three parts:

- `algod.top`, a terminal view in the style of `top`.
- `algod.top daemon`, which collects metrics, status and log events from each node and keeps them for as long as you choose.
- A web UI, served by the daemon from files embedded in the binary.

Capturing data comes first. The daemon never drops or delays a sample to serve a viewer. The user documentation is at https://algod.top/docs/, and [DESIGN.md](DESIGN.md) explains every decision. The store format is in [docs/format.md](docs/format.md).

## Install

```
go install algod.top@latest   # installs the algod.top command
go run algod.top@latest       # runs it without installing
```

The binary has no cgo and no runtime dependencies. It runs on Linux, macOS and Windows. Go 1.27 builds it. Signed binaries for every platform are on the [latest release](https://github.com/txngroup/algodtop/releases/latest), with `checksums.txt` and its signature.

`algod.top upgrade` installs the newest release on this machine and on every agent the hub runs. Each one checks the release signature itself and restarts onto the new binary.

## Quick start

```
algod.top                          # finds local nodes and shows them live
algod.top -d ~/.mainnet            # one data directory
algod.top -u http://host:8080      # a REST URL; tokens from $ALGOD_TOKEN and $ALGOD_ADMIN_TOKEN
algod.top -u ssh://relay1/~/.mainnet   # a remote node over SSH, nothing installed there
```

Without a daemon the terminal UI shows live values only. To keep history:

```
algod.top daemon --discover        # find local nodes, write ~/.config/algodtop/config.json, run
algod.top                          # now shows the daemon's data, with history
```

To start the daemon at login or boot instead, run `algod.top service`. It explains the service for your platform: a launchd agent on macOS, a systemd user unit on Linux, or a system service like `algorand.service` when algod runs as its own account. `algod.top service install` sets it up.

The web UI listens on http://127.0.0.1:8090 by default. Press `?` in the terminal UI for its keys. `algod.top -b` prints the node table once, and `-n N -i SECS --json` suit scripts and cron.

## Commands

| Command | Does |
|---|---|
| `algod.top` | Terminal UI |
| `algod.top daemon` | Collector, store, health, alerts, API and web UI |
| `algod.top agent` | Collector and local buffer on a remote host |
| `algod.top export` | Series, events or sessions as CSV or JSON |
| `algod.top silence` | Add, list or end alert silences |
| `algod.top stdio [--agent]` | Connects stdin and stdout to the local socket, for use over SSH |
| `algod.top remote install\|add\|upgrade\|sync\|remove HOST` | Manage an agent over SSH |
| `algod.top service [install\|uninstall\|status]` | Start the daemon at login or boot |
| `algod.top reload` | Reload the config (the same as SIGHUP) |
| `algod.top passwd [--read-only\|--token]` | Set the web password or make a read-only token |
| `algod.top check` | Validate the config, reach each node, project disk use |

## Config

The config is one JSON file, by default `~/.config/algodtop/config.json` (`%APPDATA%\algodtop\config.json` on Windows). It holds tokens and the password hash, so keep it mode 0600. A minimal file:

```json
{
  "nodes": [
    {"name": "mainnet", "datadir": "~/.mainnet", "accounts": ["ABC...XYZ"]},
    {"name": "relay2", "url": "ssh://algorand@relay2/var/lib/algorand"}
  ],
  "retention": {"raw": "30d", "1m": "1y", "10m": "5y", "1h": "forever"},
  "alerts": [{"name": "down", "expr": "down or stalled", "for": "1m", "channels": ["phone"]}],
  "channels": {"phone": {"type": "webhook", "url": "https://ntfy.sh/my-topic"}}
}
```

Alert expressions are fixed functions such as `lag > 30s`, `peers_out < 1`, `health >= warn` or `rule:key_expiry`. Channels are `webhook`, `smtp` and `command`. They live only in the config file: neither front end can create or change one.

## Remote hosts

A hub reaches nodes on other hosts through the system `ssh`, which honours `~/.ssh/config`, ProxyJump and ssh-agent. algod.top always uses `BatchMode=yes`, so run `ssh HOST` once to accept a new host key.

- `ssh://host/<datadir>` needs nothing installed on the host. It records gaps while the hub is offline.
- `algod.top remote install HOST` copies a signed release to `~/.local/bin/algodtop`, writes an agent config from discovery, and installs a systemd user unit or a launchd agent. The agent buffers 7 days, so the hub catches up after an outage with no gap. When algod runs as another account, as package installs do, install the agent on the host as a system service with `sudo algod.top service install --system --agent`. Then point the hub at it with `algod.top remote add HOST` and `algod.top reload`.
- `algod.top -H ssh://host` opens the terminal UI of the daemon on another host with no open ports.

## Security

The unix socket is mode 0600 and needs no password. The TCP listener binds to localhost. A non-loopback address needs a password (`algod.top passwd`), and every state-changing request over TCP needs a CSRF token. algod tokens never reach a client, and the algod client sends only GET requests to a fixed allowlist.

## Development

```
go test ./...                     # unit and end-to-end tests
go test -race ./...               # what CI runs
go test ./internal/tui -update    # rewrite terminal UI golden files
test/remote/run.sh                # SSH integration test in Docker (real sshd)
ALGODTOP_FOOTPRINT=1h go test ./internal/daemon -run TestFootprint -v   # resource budget
go run ./internal/tools/algodtop-gen -dir /tmp/s -nodes 5 -days 30      # synthetic history and query timings
```

The web smoke test in `internal/web` runs when Chrome or Chromium is installed. `ALGODTOP_SMOKE_SHOT=DIR` saves a screenshot of each page. `ALGODTOP_CPUPROFILE=path,seconds` and `ALGODTOP_MEMPROFILE=path` make a running daemon write CPU and allocation profiles.

`site/` is the https://algod.top website. `.github/workflows/pages.yml` publishes it to GitHub Pages on every push to `main` that changes it. Its pages carry the `go-import` tags that let `go install algod.top/algodtop@latest` find this repo.

`internal/fakealgod` is an algod stand-in that serves a Mainnet-shaped `/metrics` fixture, produces rounds, churns peers, writes `node.log` and can restart, stall and rotate logs. `go run ./internal/fakealgod/cmd/fakealgod -d /some/dir` runs it alone.

Every merge to main publishes a signed GitHub release from the `release` job in `.github/workflows/ci.yml`. DESIGN.md section 16 describes it and the release key. `internal/release/cmd/algodtop-sign` makes the key and signs and verifies a release directory:

```
go run ./internal/release/cmd/algodtop-sign keygen release.key        # prints the public key
go run ./internal/release/cmd/algodtop-sign sign release.key dist algodtop_linux_amd64 ...
go run ./internal/release/cmd/algodtop-sign verify dist algodtop_linux_amd64 ...
```

The open decisions in DESIGN.md section 22 are still open, among them the approval of `golang.org/x/sys/windows/svc` for the Windows service handler.

## License

algod.top is a txn.group project under the MIT license. See [LICENSE](LICENSE), and [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md) before you send a change or a report.
