# algodtop config reference

The config is one JSON file. Unknown keys are an error, so a typo fails at load rather than being ignored. Durations are strings such as `10s`, `5m`, `30d`, `1y` or `forever`. Paths accept `~` and forward slashes on every platform.

| Platform | Config file | Data directory |
|---|---|---|
| Linux | `$XDG_CONFIG_HOME/algodtop/config.json`, default `~/.config/algodtop/config.json` | `$XDG_DATA_HOME/algodtop`, default `~/.local/share/algodtop` |
| macOS | `~/.config/algodtop/config.json` | `~/Library/Application Support/algodtop` |
| Windows | `%APPDATA%\algodtop\config.json` | `%LOCALAPPDATA%\algodtop` |

`$ALGODTOP_CONFIG` overrides the config path. An agent reads `agent.json` next to the daemon config.

## Top level

| Key | Default | Meaning |
|---|---|---|
| `store_dir` | the data directory above | algodtop's own data |
| `socket` | `<store_dir>/algodtop.sock` | API socket for the terminal UI |
| `nodes` | | Monitored algod nodes |
| `remotes` | | Hosts running an algodtop agent |
| `web` | | TCP listener and auth |
| `retention` | | Guaranteed window per tier |
| `disk` | | Disk floor |
| `intervals` | | Collection intervals |
| `caps` | | Series caps |
| `alerts`, `channels`, `heartbeat` | | Alerting |
| `logs` | | Log ingestion |
| `ntp` | | Clock offset check |
| `runtime` | | Go runtime limits |
| `agent` | | Agent mode settings |

## nodes

| Key | Meaning |
|---|---|
| `name` | Letters, digits, `.`, `_` and `-` |
| `datadir` | A local data directory. algodtop reads `algod.net`, `algod.pid`, the tokens, `config.json`, `genesis.json` and `node.log` from it. |
| `url` | Instead of `datadir`: `http(s)://host:port`, or `ssh://[user@]host[:port]/<datadir>` for SSH without install |
| `token`, `admin_token` | Tokens for `url` nodes. Prefer the file forms. |
| `token_file`, `admin_token_file` | Files holding the tokens |
| `accounts` | Addresses to watch for online status, vote key and incentive eligibility |
| `scrape_interval`, `peers_interval` | Per-node overrides |
| `backfill_logs` | Read the existing `node.log` and archive on first start |
| `keep_raw_log_days` | Keep info log lines raw for this many days, for an investigation |
| `disabled` | Keep the entry but do not collect |

The admin token is needed for `/v2/node/peers` and `/v2/participation`. It is sent only to those two endpoints.

## remotes

| Key | Meaning |
|---|---|
| `name` | Prefix for the remote's nodes, as in `relay1/mainnet` |
| `host` | SSH agent: an `~/.ssh/config` alias or `ssh://user@host:port` |
| `agent_path` | Absolute path of the agent on the remote, default `~/.local/bin/algodtop` |
| `url` | TLS agent: `https://host:port` |
| `token_file` / `token` | TLS agent token (see `algodtop passwd --agent-token`) |
| `ca_file` | The agent's certificate or CA, to pin a self-signed agent |
| `cert_file`, `key_file` | Client certificate for mutual TLS |

## web

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8090` | Web UI address. `""` disables the listener. |
| `tls` | | `self-signed` to generate and keep a certificate |
| `tls_cert`, `tls_key` | | A certificate to use instead |
| `password_hash` | | Set with `algodtop passwd` |
| `read_only_password_hash` | | Set with `algodtop passwd --read-only` |
| `read_only_token_hashes` | | Set with `algodtop passwd --token` |
| `session_ttl` | `7d` | Login session lifetime |
| `allow_unauthenticated_remote` | false | Allow a non-loopback listener with no password |
| `allow_config_edits` | false | Let the Settings page edit nodes and retention |

## retention

| Key | Default |
|---|---|
| `raw` | `30d` |
| `1m` | `1y` |
| `10m` | `5y` |
| `1h` | `forever` |
| `peer_addresses` | `1y`, after which sessions keep only /24 aggregates |

Shortening a window deletes data at the next maintenance pass, and the deletion is recorded as an event. `algodtop check` previews it.

## disk

| Key | Default | Meaning |
|---|---|---|
| `floor_percent` | 5 | Free space below which algodtop deletes its oldest data |
| `at_floor` | `delete` | `pause` stops writing instead and never deletes inside a window |

## intervals

`scrape` (10s), `peers` (30s), `participation` (5m), `accounts` (5m), `supply` (10m), `proc` (10s), `proc_net` (60s), `group_commit` (1s).

## caps

`series_per_metric` (200), `series_per_node` (5000), `templates_per_node` (2000), `flood_app_ids` (100). New series beyond a cap are dropped and counted, and the node health names the metric.

## alerts

```json
{"name": "stall", "expr": "down or stalled", "for": "1m", "cooldown": "2m", "repeat": "1h",
 "severity": "critical", "channels": ["phone"], "nodes": ["mainnet"]}
```

Expressions:

- Booleans: `down`, `stalled`, `catchup`, `agent_unreachable`, and `rule:<name>` for any health rule (`down`, `health`, `stalled`, `disk`, `key_expiry`, `account_offline`, `key_mismatch`, `no_votes`, `clock`, `catchup`, `key_expiry_warn`, `incentive_lost`, `disk_fill`, `no_outgoing`, `error_rate`, `version_behind`, `algodtop`).
- Comparisons `<var> <op> <value>` with `>`, `>=`, `<`, `<=`, `==`, `!=`. Variables: `lag`, `round_time`, `round_p99`, `cpu`, `rss`, `disk_free_pct`, `disk_days`, `peers`, `peers_out`, `peers_in`, `pool`, `tpb`, `error_rate`, `clock_offset`, `key_days`, `net_in`, `net_out`, `health`. Values take units: `30s`, `500ms`, `4GiB`, `10%`, or `warn` and `critical` for `health`.
- `a or b` combines two expressions.

An alert fires when its expression holds for `for`, clears after it is false for `cooldown`, and repeats every `repeat` while firing. Silences suppress notifications but keep the events.

## channels

Channels live only in the config file. The web UI and the API cannot create or change one, because a command channel runs a program as the daemon's user.

```json
"channels": {
  "ntfy":  {"type": "webhook", "url": "https://ntfy.sh/topic", "template": "{{.Node}}: {{.Message}}"},
  "slack": {"type": "webhook", "url": "https://hooks.slack.com/...", "template": "{\"text\": {{json .Message}}}"},
  "mail":  {"type": "smtp", "smtp": {"addr": "smtp.example.com:587", "from": "algodtop@example.com", "to": ["ops@example.com"], "username": "u", "password_file": "~/.smtp"}},
  "mac":   {"type": "command", "command": ["osascript", "-e", "display notification \"algodtop alert\""]}
}
```

A webhook posts the notification JSON (`alert`, `node`, `state`, `severity`, `message`, `since`, `time`, `host`, `expr`) unless a Go `text/template` is given. A command gets the JSON on stdin and `ALGODTOP_ALERT`, `ALGODTOP_NODE`, `ALGODTOP_STATE` and `ALGODTOP_MESSAGE` in its environment.

`heartbeat.url` is pinged every `heartbeat.interval` (1m), for a dead-man switch on any healthchecks-style service.

## logs, ntp, runtime, agent

| Key | Default | Meaning |
|---|---|---|
| `logs.backfill` | false | Backfill existing logs on first start, for every node |
| `logs.keep_raw_info_days` | 0 | Keep info lines raw for every node |
| `logs.disable` | false | Turn the tailer off |
| `ntp.server` | `pool.ntp.org` | SNTP server for the clock offset |
| `ntp.interval` | `10m` | |
| `ntp.disable` | false | |
| `runtime.gomemlimit_mib` | 96 | Go memory limit |
| `runtime.gomaxprocs` | 2 | |
| `runtime.no_low_priority` | false | Keep the default process priority |
| `agent.buffer` | `7d` | How long an agent keeps pulled data |
| `agent.buffer_cap_mib` | 1024 | Size cap for unpulled data |
| `agent.listen` | | TLS agent listener, for hosts the hub cannot reach over SSH |
| `agent.token_hash` | | Set with `algodtop passwd --agent-token` |
| `agent.tls_cert`, `agent.tls_key` | self-signed | |
| `agent.client_ca` | | Accept hub client certificates signed by this CA |
