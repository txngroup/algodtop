# algod.top design

algod.top is a small, self-hosted monitor for one or more algod nodes. It has three parts in one binary:

- A daemon that collects metrics, status and log events from each node and stores them indefinitely, or for as long as the operator chooses. It does nothing else, and it stays as light as possible.
- `algod.top`, a terminal view in the style of Unix `top`, for operators who live in a shell.
- A web UI, served by the daemon as static files, for operators who prefer a browser.

The project lives at `algod.top`.

Capturing and storing data is priority one. The daemon never drops or delays a sample to serve a viewer. Reviewing history is something an operator does when needed, from either front end.

This document covers what the daemon collects, how it stores the data, how the two front ends present it, and the decisions still open.

## 1. Background

Watching a Mainnet node closely with shell scripts, CSV files and manual log greps shows six things that matter, and they set the requirements here:

- 30 s samples of chosen Prometheus series, with rates computed from counters.
- 60 s bandwidth windows per process, separate from algod's own byte counters. The two can disagree by more than 100 KB/s.
- Log-derived events: disconnect reasons, inbound/outbound direction, remote address, and "connection reset" errors.
- Annotations marking each run (config change, restart) so before/after comparisons are clear.
- Correlation with Mainnet load (transactions per block, invalid-transaction floods). This separated network effects from local ones.
- Per-peer history. Groups of peers that reset together only show when disconnects are grouped by remote IP over hours.

algod.top should do all of this continuously and show it without anyone writing awk.

## 2. Goals and non-goals

Goals:

- Capture every useful sample, event and log signal, and never lose data inside its guaranteed retention window (section 8). The one exception is a nearly full disk, where new data wins over old (section 18).
- Keep the daemon small: low CPU, low RSS, low I/O, and no work done for viewers who are not connected (section 16).
- Run on Linux, macOS and Windows with the same metrics, views and commands. algod has no official Windows binaries yet, but nodes run on Windows today through go-algo-win (github.com/GalaxyPay/go-algo-win), a Windows build of go-algorand that the FUNC node manager installs and runs. Every collector sits behind a platform interface from the start (section 5).
- Monitor several algod instances, on the local machine and on other hosts. Figures in this document marked "measured" come from Mainnet-sized test nodes.
- Keep history indefinitely by default, or for whatever range the operator configures. Recent data stays at full resolution, and older data is kept at reduced resolution.
- Answer "is every node healthy, and if not, why" within a few seconds, from both the terminal and the browser.
- Answer "what changed" questions by overlaying events, annotations and comparisons on the history.
- Use the Go standard library, the approved dependencies in section 15, and hand-written HTML, CSS and JavaScript. Any other dependency needs approval first.
- Ship one self-contained binary per platform, built with `CGO_ENABLED=0`, with the web UI embedded.
- Stay read-only toward algod. algod.top never calls an endpoint that changes node state.

Non-goals:

- A general-purpose TSDB or a Prometheus replacement. The storage engine serves algod.top's query patterns only.
- PromQL or any query language. The front ends offer fixed aggregations (rate, avg, min, max, sum by label, percentile from histogram).
- Indexer, conduit or relay-fleet management.
- Remote control of nodes (restart, config edits, key generation).

## 3. Architecture

```
                +----------------------- algod.top daemon ------------------------+
                |                                                                |
 algod REST --->| collectors (per node)     storage writer      API server       |<--- algod.top TUI
 /metrics ----->|  - metrics scraper  --->  - WAL               - /api/* JSON    |     (unix socket)
 node.log ----->|  - round watcher    --->  - per-node heads    - /api/stream    |
 process ------>|  - log tailer       --->  - 2 h raw blocks    - /ui/* static   |<--- browser
 host --------->|  - process/host           - partial rollups   - /metrics self  |     (TCP, optional)
                |  - change detector        - compactor, scrub                   |
                |                           - events, sessions                   |
                |                                                                |
                | health + alert engine <-- recent samples, events --> webhook   |
                +----------------------------------------------------------------+
```

The daemon is one process. Each node gets its own collector goroutines, one goroutine owns all storage writes, and an HTTP server answers queries. Collectors send batches to the writer over a channel. Queries read immutable block files plus the in-memory head, so they never block writes.

The daemon has two listeners:

- A unix socket at `<data>/algodtop.sock`. The terminal UI uses it. Filesystem permissions are the access control, so no password is needed. On Linux and macOS the socket is mode 0600, or 0660 for the group in `socket_group`. Windows 10 1803 and later support unix sockets, and Go's `net` package handles them there. On Windows the socket lives under `%LOCALAPPDATA%`, whose ACL already limits it to the user, SYSTEM and Administrators.
- An optional TCP listener for the web UI and remote clients, default `127.0.0.1:8090`. Setting `web.listen` to empty disables it.

Both listeners serve the same API. The daemon renders nothing itself. The web UI renders in the browser, and the terminal UI renders in its own process. An idle daemon with no viewers does only collection, storage and health evaluation.

Remote hosts (section 12) are reached over SSH, either directly to algod with nothing installed, or through the same binary running in agent mode. An agent collects locally and the hub pulls from it.

## 4. Nodes and discovery

A node is identified by a user-chosen name (for example `mainnet-1`) plus the genesis ID and hash read from the node. The genesis hash stops a data directory reused for another network from merging two histories.

For a local node, the config only needs the data directory. algod.top reads these files from it:

| File | Use |
|---|---|
| `algod.net` | REST address, for example `127.0.0.1:14001`. Re-read on every restart, because algod can pick a new port. |
| `algod.token`, `algod.admin.token` | API tokens. The admin token is needed for `/v2/node/peers` and `/v2/participation`. When `DisableAPIAuth` is true (the Mainnet node has it set), algod.top sends no token. A missing token file is not an error. |
| `algod.pid` | Process ID for CPU, RSS, FD and per-process network stats. A PID change means a restart. Absent when algod is stopped. |
| `config.json` | Hashed and diffed key by key on change. An absent file means all defaults. See "Config changes" below. |
| `genesis.json` | Network identity. |
| `node.log`, `node.archive.log` | Log tailing. `LogFileDir`, `LogArchiveDir` and `LogArchiveName` in config.json can move or rename these files, so algod.top resolves them from the config. |

A remote node is reached by REST URL and tokens, or over SSH (section 12). Process, log and host data for a remote node need an agent on that host.

algod.top can offer auto-discovery as a convenience. It would scan `$ALGORAND_DATA` and the platform's usual locations for directories that contain `algod.net` and `genesis.json`, and show what it finds for the user to confirm. On Linux and macOS the usual locations are `~/.*` and `/var/lib/algorand`. FUNC keeps each node in `<app>/algorand` and `<app>/voi`, where `<app>` is `%ProgramData%\func` on Windows, `/usr/local/share/func` on macOS and `/usr/share/func` on Linux, unless `<app>/<name>.data` names another parent directory. Discovery reads those too, and on Windows it reads `-d` from each `algod.exe`'s command line through `NtQueryInformationProcess`, which an administrator or LocalSystem may do for an algod that FUNC runs under its own service account. It never adds a node silently.

The file names above come from go-algorand, so the Linux and macOS builds and go-algo-win's Windows build share them.

### Config changes

algod reads `config.json` only at startup. algod.top therefore records two events:

- "Config pending" when the file changes on disk, with the key diff against the running config.
- "Config effective" at the next restart, with the diff against the previous effective config.

Automatic annotations and run boundaries use the effective event, for example `P2PHybridIncomingConnectionsLimit: 4 -> 40` at the moment algod restarted with it.

## 5. Data sources

| Source | What | Default interval | Notes |
|---|---|---|---|
| `GET /metrics` | All algod and libp2p Prometheus series | 10 s | No auth needed. Measured: Mainnet returns 1079 series under 407 family names in about 160 KB. 963 series are counters and 116 are gauges. |
| `GET /v2/status/wait-for-block-after/{round}` | Last round, time since last round, catchup state, upgrade state | long-poll, one per round | Returns as soon as the next round lands, or after 1 minute. This gives exact local arrival time per round with one request per round, rather than a 5 s poll. |
| `GET /health`, `GET /ready` | Liveness and readiness | 10 s | `/ready` fails during catchup, which is useful for alert state. |
| `GET /versions` | Build version, genesis ID and hash | at start and after restart | A version change gets an annotation. |
| `GET /v2/node/peers` | Current peers with address, direction and network type (ws/p2p). Builds with per-peer usage also give peer ID, supported protocols, active streams and approximate bytes per peer. | 30 s | Admin token. Diffs between polls give peer sessions even when the log level hides connects. See "Per-peer usage" in section 6. |
| `GET /v2/participation` | Keys with first, last and dilution, vote key, plus last vote and proposal rounds | 5 min | Admin token. Drives key-expiry and key-mismatch checks. |
| `GET /v2/accounts/{addr}?exclude=all` | Online status, vote key, incentive eligibility, last proposed and last heartbeat rounds, balance | 5 min | Only for accounts the user lists. `exclude=all` skips asset and app holdings. |
| `GET /v2/blocks/{round}?header-only=true` | Timestamp and `TxnCounter` (`tc`), which give transactions per block | each new round | Fetched right after the round watcher sees a new round. One request per round, about 1 per 2.8 s. After a daemon outage, missed headers are backfilled at a capped rate, as far back as the node still holds blocks. |
| `GET /v2/transactions/pending?max=1` | Pool size from `total-transactions` | 10 s | `max=0` returns every pending transaction, which is megabytes during a flood. `max=1` returns one. No pool-size gauge exists in `/metrics`. |
| `GET /v2/ledger/supply` | Online stake | 10 min | Context for consensus charts, and the basis for expected vote intervals (section 10). |
| `node.log` tail | Structured events and message counts | continuous | See section 7. |
| Process stats | CPU, RSS, threads, open FDs, disk I/O, per-process network bytes | 10 s, network 60 s | Platform-specific, see below. |
| Host stats | Disk free on the data dir volume, data dir size by subdirectory, interface bytes, load, clock offset | 10 s to 10 min | Clock offset uses a small SNTP client on the stdlib `net` package. Consensus is sensitive to clock skew. |

### Process and host stats by platform

Everything except process and host stats is platform-neutral: algod's REST API, `/metrics`, `node.log` and SNTP work the same everywhere.

Process and host stats go through one Go interface with an implementation per platform. Each stat has one canonical series name on every platform, for example `proc_cpu_seconds_total`, `proc_resident_bytes` and `proc_open_handles`. A `method` label records how it was measured. Charts, health rules and the terminal UI only know the canonical names, so they behave the same on every platform.

| Canonical stat | Linux | macOS | Windows |
|---|---|---|---|
| CPU time | `/proc/<pid>/stat` | `ps time=` | `GetProcessTimes` |
| Resident memory | `/proc/<pid>/status` | `ps rss=` | `GetProcessMemoryInfo` (working set) |
| Threads | `/proc/<pid>/status` | `ps -M` line count | Toolhelp32 snapshot |
| Open FDs or handles | `/proc/<pid>/fd` | `lsof`, every 5 min | `GetProcessHandleCount` |
| Disk I/O bytes | `/proc/<pid>/io` | not available | `GetProcessIoCounters` |
| Process network bytes | sock_diag (to spike), else interface totals | `nettop` | interface totals |
| Interface bytes | `/proc/net/dev` | `netstat -ib` | `GetIfTable2` |
| Disk free | `syscall.Statfs` | `syscall.Statfs` | `GetDiskFreeSpaceExW` |
| Load average | `/proc/loadavg` | `sysctl -n vm.loadavg` | not available |
| Restart detection | PID change, process start time | PID change, process start time | PID change, process creation time |

A stat that a platform cannot provide is shown as "not available on <platform>" in both front ends. It is never hidden, and a health rule that depends on it is listed as disabled with the reason.

Linux notes:

- All reads use the stdlib.
- Per-process network bytes are not in `/proc` for a single process. `/proc/<pid>/net/dev` covers the whole network namespace. cgroup v2 has no per-cgroup byte counters, and nftables counters need root.
- The stdlib option to spike is sock_diag over netlink, through the `syscall` package. It returns `tcp_info` per socket, including `bytes_acked` and `bytes_received`. algod.top maps socket inodes to the process through `/proc/<pid>/fd`. It misses bytes from sockets that closed between polls, and it does not cover UDP or QUIC.

macOS notes:

- The stdlib has no process-info API. algod.top runs one `ps -o pid=,rss=,time=,... -p PID1,PID2,...` for all nodes at once. It computes CPU from the cumulative `time=` field, because `%cpu` on macOS is a decaying average.
- Disk I/O per process needs `proc_pid_rusage` in libproc, which needs cgo.
- For network, algod.top runs one `nettop -L 1` for all algod PIDs every 60 s. A single sample prints cumulative bytes per process at once, in about 0.1 s. A long-lived `nettop` in logging mode was tried first, but through a pipe its output sits in a stdio buffer and arrives late.

Windows notes:

- Every call in the table is a plain Win32 API in `kernel32`, `psapi` or `iphlpapi`. No exec is needed, so Windows is cheaper to collect from than macOS. The calls go through `golang.org/x/sys/windows`, which is already in the module graph via `golang.org/x/term` (section 15). The stdlib fallback is `syscall.NewLazyDLL`.
- Per-connection byte counts (`GetPerTcpConnectionEStats`) need Administrator rights to switch on, and algod.top never needs elevation. Process network bytes therefore use interface totals, labelled as such.
- Windows has no load average. The Resources section shows CPU utilisation in its place.

Process network bytes are the one stat with no good method on every platform. A libp2p total bytes counter in algod (section 21), added to algod's existing ws byte counters, would give one cross-platform series close to wire bytes. It would still exclude transport framing and encryption.

Every chart and table that shows process network bytes names the method used.

### Self-monitoring

The daemon records its own scrape durations, missed scrapes, write latency, queue depth, CPU, RSS and disk use as series. A gap in a chart can then be explained.

### Polling cost

Scraping 160 KB of metrics every 10 s from five nodes is negligible. The log tailer is the expensive part. Measured: Mainnet's `node.log` grew to 2.5 GB in under six hours, with 3.75 million agreement trace lines. The tailer filters on raw bytes before it decodes any JSON (section 7).

## 6. Data model

### Series

A series is (node, metric name, sorted label set). algod.top stores a kind with each series: counter, gauge or histogram part.

algod does not emit a `histogram` TYPE. Each `_bucket`, `_sum` and `_count` is its own family typed `counter`, for example `# TYPE libp2p_swarm_dial_latency_seconds_bucket counter`. algodtop groups a family as a histogram when it finds `<base>_bucket` series with an `le` label plus matching `<base>_sum` and `<base>_count`. The 407 family names on Mainnet count those suffix families separately.

- Counter. algod.top stores the raw cumulative value, and the query layer computes rates. A restart resets every counter at once, so algod.top marks a reset on every counter of a node when its PID changes. A REST-only node has no PID, so a drop in most of its counters in one scrape marks the restart. A value that goes down without a restart is also treated as a reset of that series. The rate across a reset is the new value divided by the interval. Each reset is stored as an event, so a dip in a rate chart is visibly a restart.
- Gauge. algod.top stores the value as reported.
- Histogram. algod.top stores each bucket, `_sum` and `_count` as a counter. Queries derive heatmaps and percentiles (p50, p90, p99) from bucket deltas.

Series come and go. The bucket count on Mainnet changes between scrapes as new label values (dial outcomes, protocols) appear. The chunk format records presence per sample (section 8), and a series absent from a scrape is a gap, not a zero.

### Cardinality

Today no algod series carries a per-peer label, but a future version could add one. algod also puts message tags into metric names (`algod_network_p2p_received_bytes_MS`, `_PP`, `_TS` and more), so a cap per metric name does not bound the total. algod.top enforces two caps:

- Series per metric name, default 200.
- Series per node, default 5000.

New series beyond either cap are dropped, the drop is counted, and both front ends show a warning naming the metric. Per-peer data goes into the peer-session store (below), not into series.

Log-derived series have their own caps: templates per node (default 2000) and app IDs per flood counter (default 100, with the rest summed into `other`).

### Metric renames

Metric names change across algod versions. A small alias table in the binary maps old names to new ones, so a chart that spans an upgrade stays continuous. The raw stored name is kept, so nothing is lost if an alias is wrong.

### Events

An event has a time, node, kind, severity, short message, and a JSON attribute map. The kinds are:

- restart
- version change
- config pending and config effective, with the key diff
- catchup start and end
- log rotation
- counter reset
- disconnect (reason, direction, network, address)
- connect
- error and warning log lines, grouped by template
- alert fired, repeated and cleared
- silence started and ended
- monitor gap (algod.top itself was down, or paused writes)
- retention deletion (the operator shortened a window, the disk floor deleted data, or an agent buffer overflowed)

### Peer sessions

A peer session is (node, network ws/p2p, direction, remote address, P2P peer ID if known, start, end, end reason). Sessions come from two sources. Log connect and disconnect events give exact times and reasons. `/v2/node/peers` diffs fill in what the log level hides. The session store keeps the per-peer history that the reset-cluster analysis needed.

A session already open when algod.top first looks has an unknown start. It is marked open before, and the UIs show its length as a lower bound. This covers a peer in the first poll and a log disconnect with no connect before it. Without the mark, such a disconnect read as a session of 0 s.

### Per-peer usage

Some algod builds extend `/v2/node/peers` with five optional fields per peer. Released versions do not have them yet. algod.top reads them when they are present, so per-peer traffic appears as soon as a node runs a build that has them.

| Field | Meaning |
|---|---|
| `peer-id` | libp2p identity. p2p peers only. |
| `supported-protocols` | Protocols the remote reports through libp2p identify. p2p peers only. |
| `active-streams` | Protocol IDs of open streams. A ws peer reports `ws-gossip/<version>`. An empty string is a stream still negotiating. |
| `total-bytes-received`, `total-bytes-sent` | Approximate bytes exchanged with the peer |

algod.top detects support per peer entry by the presence of `active-streams`, not by algod version. The branch sends that field, even as an empty list, for every peer that reports usage. An older algod omits it, and so does a peer type that does not report usage. No config change is needed after an upgrade.

The field semantics shape how algod.top stores them:

- The byte totals are cumulative and reset when the last connection to the peer closes. algod.top treats them as a counter per peer session. The rate is the delta divided by the poll interval, and a new session starts from zero. A total that drops within a session is treated as a reset, because a fast reconnect can keep or lose totals.
- For p2p peers, the totals cover every connection to the peer ID, and each connection repeats the same totals. algod.top counts each peer ID once before summing. For ws peers, the totals are per connection.
- The bytes are counted at the stream layer, so they exclude transport framing and encryption. ws totals count gossip payload only. The sum over all peers is therefore another app-level figure, not wire bytes.
- Traffic between the last poll and a disconnect is lost, up to one poll interval. The peers poll stays at 30 s by default. Operators who want finer per-peer traffic can lower it.

Per-peer data stays out of the series index, so it cannot inflate cardinality. A peer usage store keeps one sample per poll per session: time, bytes received, bytes sent and open stream protocols. It uses 2 h delta-encoded blocks per node, like the chunk store. The protocol list is stored once per session and again only when it changes, because the response repeats it on every poll. When a session ends, its byte totals go into the session record.

Retention follows the data it describes. Per-poll samples are kept for 30 days, like the raw tier. Session totals are kept with the session for 1 year, then reduced to the per-/24 aggregates in section 17. With 100 peers polled every 30 s, the store holds about 1.7 MB per node per day, or about 52 MB per node for 30 days.

### Annotations

An annotation is a user-written note with a time range, for example "run 3: P2PHybridIncomingConnectionsLimit 40". Automatic annotations come from effective config changes and version changes. Users add and edit annotations from either front end. An annotation can carry a silence (section 11).

## 7. Log ingestion

### Tailing and rotation

algod writes `node.log` and renames it to the archive name at `LogSizeLimit`. With a large `LogSizeLimit`, such as 5 GB, a rotation can come at any hour and overwrites the previous archive. `LogArchiveName` can also be a time template, which produces many archive files instead of one.

The stdlib has no inotify, kqueue or `ReadDirectoryChangesW` wrapper, so the tailer polls on every platform. It reads new bytes every 250 ms and stats the path every second. The tailer:

- Keeps the open file. When the path points to a different file (`os.SameFile`, which compares inodes on Unix and volume serial plus file index on Windows), it reads the old file to EOF, then opens the new `node.log` from offset 0.
- On Windows, opens the log with `FILE_SHARE_DELETE` through `CreateFile`. Go's `os.Open` omits that flag. Without it, algod.top's open handle would block algod from renaming `node.log`, and algod.top would break algod's log rotation.
- Detects truncation (size smaller than offset).
- Persists (file identity, offset, first-line hash) every few seconds. After a daemon restart it resumes where it stopped, including reading the tail of the archive if a rotation happened while it was down.
- On first start, it can backfill the existing `node.log` and archive. On a busy Mainnet node that can be several GB, such as a 5 GB archive plus the live file. Backfill is optional and runs under the background I/O limit (section 16).

### Parsing

Each line is JSON. The tailer runs a byte-level prefilter first, because the noisy lines are known:

- Lines with `"file":"trace.go"` are agreement telemetry. algod.top decodes only a few useful message types from them (BlockAccepted, RoundConcluded) and counts the rest without decoding.
- "Pending transaction in pool no longer valid" floods (100k lines in 10 minutes are possible) are counted per minute and per app ID, and never stored line by line.
- HTTP access lines are counted by path and status.

The remaining lines become message templates. The tailer replaces addresses, numbers, base32 IDs, hashes, durations and peer IDs with placeholders. Each template gets a per-minute count, stored as a series in the chunk store under the template caps (section 6). This keeps "what is the node complaining about" cheap to chart over the whole history.

Specific extractors turn known lines into events with attributes. Each extractor is a small Go function with a test fixture taken from a real log. The first set covers:

- `/Network/ConnectPeer` and `/Network/DisconnectPeer`: Address, Incoming, Reason, MessageDelay. These lines appear in `node.log` even with telemetry disabled in `logging.config`.
- `wsPeer` read and write errors, including "connection reset by peer", with the remote address.
- `Made outgoing connection` and `Accepted incoming connection`.
- `using protocol /algorand-ws/2.2.0 with peer`, which marks gossip stream setup.
- `wsStreamHandlerV22: error reading peer meta headers`, the handshake EOFs.
- Catchup start and end, ledger errors, panic, and fatal.

Warning and error lines are kept raw for 90 days for drill-down. Info lines are not kept raw by default. A per-node setting can keep all raw lines for N days for an investigation.

## 8. Storage engine

### Layout

```
<data>/
  meta.json                     format version, created, node registry
  algodtop.sock                 local API socket for the terminal UI
  wal/                          append-only write-ahead log segments (64 MB)
  series/index.bin              series ID <-> (node, name, labels), append-only + periodic compaction
  chunks/raw/2026-11-03/        2 h blocks, one file per node per block
  chunks/1m/2026-11/            partial blocks (2 h), compacted into 1 day blocks
  chunks/10m/2026/              partial blocks (2 h), compacted into 1 week blocks
  chunks/1h/2026/               partial blocks (2 h), compacted into 1 month blocks
  events/2026-11.log            append-only, length-prefixed records, CRC32 per record
  sessions/2026-11.log          same format
  peers/2026-11-03/             per-session usage samples, 2 h blocks per node
  annotations.json              small, rewritten atomically (write temp, fsync, rename)
  state/                        log tail offsets, node watermarks, alert and silence state
```

### Chunk format

One block holds every series for one node over one time window. Inside a block:

- One timestamp column per collector group. The metrics scrape, process stats, network stats, round watcher and log counters each sample on their own clock, so each gets its own column. Timestamps are delta-of-delta encoded, so a regular 10 s interval costs almost nothing.
- One value column per series with Gorilla XOR float encoding. Unchanged values cost one bit, and most algod series barely change. Encoder, decoder and bit stream together are a few hundred lines of Go.
- A presence bitmap per series against its group's timestamp column, run-length encoded. A series that appears halfway through a block, or misses a scrape, has gaps rather than invented values. An all-present series costs a few bytes.
- A footer with a series offset table, min and max time, and a CRC32 of the block.

Blocks are immutable once written.

### Head and WAL

Each node has its own head: the newest 2 h of raw samples, kept in memory in the same compressed encoding as a block. Head windows align to 2 h UTC boundaries, so every 1m, 10m and 1h bucket falls inside exactly one head. A Mainnet-sized node needs about 1.2 MB of head. The writer appends to per-series buffers under a per-node lock held for microseconds. Readers copy the buffer slice under the lock and decode outside it.

Every sample also goes to the WAL. The writer fsyncs the WAL once per second (group commit, configurable). After a crash, startup replays the WAL. A torn final record fails its CRC and is discarded. The cost is at most the last second of samples.

When a node's head window closes, or on a clean shutdown, the writer flushes the head to a raw block and fsyncs it. In the same pass, it computes the 1m, 10m and 1h aggregates for that window from the in-memory head and writes them as partial rollup blocks. No rollup ever re-reads raw blocks from disk.

A head also flushes when its window ended more than a grace period ago (default 10 minutes) by the hub's clock, even if no newer sample arrived. A node that went quiet, such as an unreachable remote, therefore never holds its head open. Samples that arrive later for that window take the late-data path.

The writer deletes a WAL segment once every head with records in it has flushed. Without the idle flush, one quiet node would pin every WAL segment written after it went quiet.

Each node has a watermark: the newest sample time the hub has for it. Heads advance per node on that node's sample time, not on the hub's wall clock. A remote node whose agent was unreachable for a day keeps its own head and catches up without disturbing other nodes (section 12).

### Late data

Data older than a node's current head is late. It arrives after an agent outage or a host clock jump. The writer puts it into a supplementary raw block for its window and rewrites the partial rollups for that window. Queries merge supplementary blocks with the main ones, and compaction folds them in.

### Tiers and retention

| Tier | Resolution | Guaranteed window | Aggregates per bucket |
|---|---|---|---|
| raw | scrape interval (10 s) | 30 days | value |
| 1m | 1 minute | 1 year | see below |
| 10m | 10 minutes | 5 years | see below |
| 1h | 1 hour | forever | see below |

These windows are defaults. Each one is a config value: any duration, or `forever`. With the defaults, history never ends. It only gets coarser with age, and the 1h tier keeps everything.

Aggregates in the rollup tiers depend on the series kind:

- Gauges keep min, max, sum, count and last. Min and max let a long-range chart show spikes rather than averaging them away. Charts draw the min/max range as a band behind the average line.
- Counters keep `last` and `max_rate`. `last` is a reset-adjusted cumulative value, so rates computed from two 1h buckets across a restart are correct. `max_rate` is the highest per-sample rate inside the bucket, so peak bandwidth survives on long charts.
- Histogram parts keep `last` only, which is all percentile and heatmap queries need.

Retention windows are guarantees:

- Every tier keeps every sample younger than its window. Rolling up never deletes the source. A raw sample stays at full resolution for the full 30 days, even though 1m, 10m and 1h copies of it exist from the moment its head flushes.
- For the most recent 30 days, all four tiers hold the same period. This overlap is deliberate and is included in the size estimate.
- A block is deleted only when its whole time range is older than its tier's window, and every rollup and compaction that reads it is complete and fsynced. A block can outlive its window by up to one block length, never the reverse.
- The compactor merges partial blocks into day, week and month blocks when the period closes. It removes the partials only after the merged block is fsynced. Partials and merged blocks are the same tier, so this loses nothing.
- Only the operator shortens a window, by editing the config. On reload, algod.top logs a retention deletion event listing what it removed. `algod.top check` previews the deletion before the reload.
- The disk floor (section 18) is the one exception. When the volume is nearly full, algod.top deletes the oldest data to keep capturing new data, and reports exactly what it removed.
- On Windows, a block that a query has open cannot be deleted unless it was opened with `FILE_SHARE_DELETE`. Block readers use that flag, and the deleter retries a failed delete on the next pass.

The background jobs (flush, rollup, compaction, deletion) are idempotent, so an interrupted job re-runs cleanly.

### Size estimate

The estimate uses Mainnet's series mix per node and the same pessimistic 1.5 bytes per stored value for every tier. Most series are constant, so real figures should be lower.

Values per bucket per node in the rollup tiers:

- 116 gauges x 5 aggregates = 580
- 287 plain counters x 2 aggregates = 574
- 676 histogram parts x 1 aggregate = 676
- total: 1830 values per bucket

Per node:

- raw: 1079 x 8640/day x 1.5 B = about 14 MB/day, 420 MB for 30 days.
- 1m: 1830 x 1440/day x 1.5 B = about 4 MB/day, 1.44 GB for 1 year.
- 10m: about 0.4 MB/day, 144 MB per year.
- 1h: about 66 KB/day, 24 MB per year.

Tiers with a bounded window stop growing once the window is full. Tiers kept longer grow at a steady rate. For five nodes with the default windows:

| Tier | Size for five nodes |
|---|---|
| raw (30 days) | 2.1 GB, reached after 30 days |
| 1m (1 year) | 7.2 GB, reached after 1 year |
| 10m (5 years) | grows 0.72 GB per year, up to 3.6 GB |
| 1h (forever) | grows 0.12 GB per year |

The bounded tiers settle at about 9.3 GB. After that, history adds under 1 GB per year for five nodes, and 0.12 GB per year once the 10m window is full. Setting the 1m tier to `forever` adds 7.2 GB per year for five nodes, and setting raw to `forever` adds about 25 GB per year. `algod.top check` prints the projection for the configured windows.

The 1m tier is the largest. This is the number to measure first in the prototype. If real compression is much better, the 1m tier can be kept longer.

Events and sessions are small next to series. A busy day can produce a few thousand disconnect events, roughly 1 MB. Per-peer usage adds about 0.26 GB for five nodes at its 30-day window (section 6).

### Why not SQLite or flat CSV

SQLite would make events, sessions and ad-hoc queries easier. It needs either cgo (`mattn/go-sqlite3`) or a large pure-Go port (`modernc.org/sqlite`), and both are dependencies. Flat CSV is too large at this scale, and its range queries are slow. A purpose-built chunk store is the largest piece of work in algod.top, but its scope is narrow: append by time, read by time range, roll up, delete by window.

If the custom store proves too costly, `modernc.org/sqlite` is the fallback to request. It would hold events, sessions and annotations only, and series would stay in chunk files.

### Backup and export

- Snapshot: `POST /api/admin/snapshot` (auth required on TCP) hard-links the immutable blocks and copies the heads into `<data>/snapshots/<time>/`. A plain `cp -R` or `rsync` of that directory is a full backup.
- Export: any chart offers CSV and JSON of exactly what it shows. `algod.top export --node X --metric Y --from --to --tier` exports the same data.
- The block format is documented in `docs/format.md`, with a format version, so the data stays readable without algod.top, however old it is.

## 9. Query API

The API is plain JSON over HTTP, served by stdlib `net/http` with the pattern router (Go 1.22 or later). The same handlers serve the unix socket and the TCP listener. Both accept HTTP/1.1, and the unix socket also accepts HTTP/2 without TLS for the SSH relay (section 12). State-changing requests need no login over the unix socket and need login plus CSRF over TCP (section 17).

| Endpoint | Returns |
|---|---|
| `GET /api/nodes` | Nodes with live status and health summary |
| `GET /api/series?node=&metric=&match=label:value&from=&to=&points=&tier=&fn=rate\|avg\|min\|max\|p99&by=label` | Time series |
| `GET /api/metrics?node=&q=` | Metric names and label values, for the explorer |
| `GET /api/events?node=&kind=&from=&to=` | Events |
| `GET /api/sessions?node=&addr=&from=&to=` | Peer sessions |
| `GET /api/peers/clusters?node=&window=30s&min=5` | Groups of peers that disconnected together (section 10) |
| `GET /api/logs/templates?node=&from=&to=` | Template counts |
| `GET /api/logs/lines?node=&template=&from=&to=` | Kept raw lines |
| `GET/POST/PUT/DELETE /api/annotations` | Annotations |
| `GET/POST/DELETE /api/silences` | Alert silences |
| `POST /api/admin/snapshot` | Snapshot for backup (section 8) |
| `POST /api/admin/reload` | Config reload, used by `algod.top reload` |
| `GET /agent/v1/batches` | Agent pull protocol, served by agents only (section 12) |
| `GET /api/stream?nodes=` | Server-Sent Events: live status and new samples for every node and series the client subscribes to |
| `GET /metrics` | algod.top's own metrics in Prometheus text format |

`from` and `to` take unix seconds, milliseconds or nanoseconds, told apart by size, RFC 3339, `now`, or a time relative to now such as `-6h`. A unix time may have a fraction, because JavaScript does its time arithmetic in fractional milliseconds.

### Tier selection

The `points` parameter is the number of buckets the client can draw: chart width in pixels for the web UI, or columns for the terminal UI. The planner works as follows:

1. Pick the coarsest tier whose resolution is at or below range divided by points.
2. If any part of the range is not yet in that tier (the current head, or late data still being rolled up), fill that part from the next finer tier, down to raw and the head. The newest sample is visible to queries as soon as the writer accepts it.
3. If any part of the range is older than that tier's window, fill that part from the finest tier that still holds it. A 2-hour zoom into last year's data picks raw in step 1 and gets 1m or 10m buckets here. The response marks the tier used for each part, so the front ends can label the coarser stretch.
4. Aggregate to exactly `points` buckets, each with min, max and avg.

`tier=raw` forces full resolution for any range inside the raw window. Both front ends offer a "full resolution" toggle that sets it, so an operator can always see every sample from the last 30 days. A chart of the whole history never ships millions of points, and a 1-hour chart gets raw samples without asking.

### Query limits

Queries run with a concurrency limit (default 2) and a timeout (default 10 s). A heavy query waits or fails. It never delays the writer or a collector.

### Live updates

Live updates use SSE. It is plain HTTP, the stdlib supports it with `http.Flusher`, and the browser reconnects automatically. WebSocket would need `golang.org/x/net/websocket` or a hand-written frame codec, and the clients only need server-to-client updates.

The daemon pushes each node's status when the node confirms a new round, so the Fleet page moves with the chain rather than with a timer. Nodes on one network confirm a round within moments of each other, so a push waits 150 ms and carries every node that moved. A 2 s timer covers the rest, such as CPU. On the hub, an agent node's round and lag move when its round batch arrives through the pull, between the 5 s polls of the agent's full status.

Each client opens one multiplexed stream that covers every node it shows. Browsers allow six HTTP/1.1 connections per origin. One stream per node would use five of them on the Fleet page and stall chart fetches, and a second tab would make it worse.

## 10. Derived views and health

The front ends compute these from stored data rather than storing them separately.

- Sync lag is `time-since-last-round` from the round watcher.
- Round time is the gap between local arrival times of consecutive rounds, from the round watcher, shown as mean and p99. A long-poll is an arrival time only when it went out right after the previous answer. The first answer after a start, a pause or an error comes back at once for a round that landed earlier, so it starts the next gap without closing one. Block header timestamps are whole seconds set by the proposer, so their gaps come out as 2 or 3 and are not used.
- Transactions per block is the difference in `TxnCounter` between consecutive headers. It is the "is the network busy" line overlaid on bandwidth and CPU charts.
- Bandwidth has three views, plus a fourth when per-peer usage is available. Each is labelled:
  - algod app-level bytes split into WebSocket and P2P (`algod_network_*` and `algod_network_p2p_*`), stacked
  - the process wire bytes, with the method named
  - the gap between the two, which shows libp2p overhead (DHT, gossipsub control, encryption)
  - traffic by peer, as the top N peers stacked plus "other", from per-peer usage
- Top talkers are the peers with the highest receive or send rate over the selected range, from per-peer usage.
- Idle peers are connected peers with no open gossip stream in `active-streams`. A peer that stays connected without gossip uses a slot and carries nothing.
- Peers are counts by network (ws/p2p) and direction (in/out), plus gossip streams, pubsub peers and DHT peers.
- Branch metrics are metrics that exist only on some algod builds, such as the setup timeouts and inbound evictions (busy/idle) on builds that add them. They show as rates and appear automatically when present.
- Peer clusters are groups of at least N inbound sessions that ended within W seconds, listed with the IPs involved. The view also flags IPs that appear in more than one cluster. This finds peers that reset together without anyone grouping disconnects by hand. With per-peer usage, each cluster also shows member traffic in the minutes before the disconnect. It groups members by their `supported-protocols` set, which works as a rough software fingerprint.
- Participation shows each key's validity remaining in rounds and estimated days, its last vote round and last proposal round, whether the account is online, and whether it is incentive-eligible.
- Expected vote interval for an account comes from its share of online stake (`/v2/ledger/supply`). A small account is selected less often, so "rounds since last vote" is judged against this interval rather than a fixed number. The last vote comes from the local key (`/v2/participation`). The on-chain view comes from the account's last proposed and last heartbeat rounds, which are what the network uses to judge absence.
- Suspension shows in the account as status Offline with its vote key still set. algod.top names that state "suspended" rather than "offline", because recovery differs: a suspended account comes back with a heartbeat or a new keyreg.
- Disk shows days until full, from a linear fit of the last 7 days' use, and the data dir size by subdirectory.
- Memory shows RSS with a flag if the 24 h minimum has risen for 3 days running. Plain RSS spikes are normal.

Each node has a health state of OK, Warn or Critical, and the reasons are always listed in words, for example "Warn: participation key for ABC... expires in 6 days". The defaults are:

- Critical:
  - algod is down, or `/health` fails. When a local algod stops answering while its process runs, the reason names the pid, because a hung algod needs a kill and a crashed one a start.
  - the last round is older than 30 s and the node is not in catchup. A node whose `/v2/status` fails, for example because algod rejects the API token, has no round, so its lag counts from the first failure and the reason names the error. algod answers `/metrics` and `/health` without a token, so such a node would otherwise look up and well.
  - disk is under 5%
  - an account's active participation key expires within 2 days. The active key is the on-chain vote key when the account is listed, else the valid key that voted last. A renewed account keeps its old key installed until someone deletes it, and that key does not count.
  - a listed account was online and is now offline or suspended (offline with its vote key still set)
  - a listed account is online but its installed key is not the vote key registered on chain
  - a listed online account has not voted for more than 10 times its expected vote interval
  - the clock offset is over 1 s
- Warn:
  - catchup is running
  - no round has been read yet because `/v2/status` fails. It becomes Critical once the lag passes 30 s.
  - an account's active key expires within 14 days
  - a listed account lost incentive eligibility
  - disk will fill within 14 days
  - the node has 0 outgoing peers
  - the error log rate is above its baseline
  - the version is behind the latest release seen on another node of the same network and release channel. Developer builds take no part, because their build numbers count hours rather than patches.
  - algod.top missed scrapes, dropped series, paused writes, or deleted data at the disk floor

## 11. Alerting

Alert rules live in the config file with a name, expression, duration, severity, repeat interval and channels. The expressions are fixed functions over the derived values above (for example `lag > 30s for 1m`), with no general query language. Every health rule in section 10 is also available as an alert.

The channels need only the stdlib:

- Webhook, a JSON POST. With a template it works for ntfy, Slack, Discord and Pushover.
- SMTP through `net/smtp`. `net/smtp` takes no context, so algod.top dials the server itself and sets a 30 s deadline on the connection. Without it, a server that accepts and never answers would hold the channel forever. Port 465 speaks TLS from the first byte; other ports upgrade with STARTTLS when the server offers it. Each mail carries `Date` and `Message-ID`, which spam filters expect.
- Command: algod.top runs a configured program with the alert JSON on stdin. This covers `osascript` notifications on macOS and PowerShell toast notifications on Windows.

Channels are defined only in the config file. Neither front end nor the API can create or change a channel, because a command channel runs a program as the daemon's user.

Firing needs the condition true for the full duration. Clearing needs it false for a cooldown, so alerts do not flap. A firing alert repeats at its repeat interval (default 1 h). Each transition is stored as an event.

A notification says why in the words of the health reason, so `down` names the error, such as a timeout. A cleared notification names the message the alert fired with. A node back from down is stalled until its next round, and the last message, such as "last round is 81s old", read as a new problem.

Each channel has its own delivery worker and queue. The worker keeps the channel's order, so a receiver never sees "cleared" before "repeat". A channel that hangs delays only its own notifications. A failed webhook or SMTP delivery is tried again after 10 s, 1 min and 5 min, which outlasts a webhook service's restart or a DNS blip. A failure that a retry would repeat is given up at once: a webhook 4xx other than 408 and 429, or an SMTP 5xx. A command runs once, because it may have done its work before it failed. A notification that never gets through is stored as an `alert-undelivered` event on its node, so a broken channel shows in both UIs before the incident it was meant for.

A rule that does not parse, or that repeats another rule's name, is left out, and the daemon runs the rest. At startup a `config-error` event names it. A reload with such a rule fails and keeps the running config. `algod.top check` reports these rules before the daemon sees them, and `algod.top check --notify` sends a test notification on every channel. The daemon drops the firing state of a rule removed from the config, so the rule does not stay firing in the UIs.

### Silences

A planned restart for a config change would otherwise fire "algod is down". A silence suppresses notifications for chosen nodes and rules over a time range. Health state and events are still recorded. An operator creates one with `algod.top silence --node mainnet --for 10m "restart for run 4"`, from the terminal UI, or from the web UI. An annotation can carry a silence, so marking a run and silencing its restart is one step.

### Dead-man switch

A dead-man switch needs something outside algod.top. The daemon can ping an external heartbeat URL (any healthchecks-style service) every minute. If the daemon or the machine dies, that service sends the alert.

## 12. Multiple hosts

The hub is the daemon that stores everything and serves the front ends. It reaches remote nodes in one of four modes:

| Mode | Install on the remote | Transport | Data | Hub offline |
|---|---|---|---|---|
| REST | nothing | HTTP(S) to algod | metrics and REST | gap |
| SSH, no install | nothing | system `ssh -W` to algod | metrics and REST | gap |
| SSH agent | `algod.top agent` | `ssh host algod.top stdio --agent` | everything | no gap, 7 day buffer |
| TLS agent | `algod.top agent` | HTTPS pull with a token or mutual TLS | everything | no gap, 7 day buffer |

SSH no-install is the quick path: a remote node can be added in one line with nothing to set up on the host. The SSH agent is the recommended mode for any node worth keeping full history for. The TLS agent is for hosts the hub cannot reach over SSH.

### SSH transport

algod.top runs the system `ssh` binary, rather than linking an SSH library:

- It honours `~/.ssh/config`, host aliases, ProxyJump, keys, ssh-agent and `known_hosts` with no extra setup.
- The OpenSSH client ships with macOS, nearly every Linux distribution and Windows 10 and later.
- `golang.org/x/crypto/ssh` would be a dependency and would ignore most of `~/.ssh/config`.

algod.top always passes `-o BatchMode=yes`. A host that wants a password, or whose key is not in `known_hosts`, fails with a clear error rather than prompting inside the daemon or the terminal UI. `algod.top check` reports such hosts and tells the operator to run `ssh <host>` once.

A dropped connection reconnects with backoff from 1 s up to 5 minutes. A no-install node records the outage as a monitor gap. An agent node catches up from its buffer when the connection returns.

### SSH without install

The node URL names the host and data directory, for example `ssh://relay1/~/.mainnet` or `ssh://algorand@relay2:2222/var/lib/algorand`. A leading `/~` means the remote home directory, and the host can be any alias from `~/.ssh/config`.

- At start, the hub runs `ssh host cat` on `algod.net`, `algod.pid`, `config.json` and the token files in one command. Tokens are held in memory only and never written to the hub's disk. `config.json` gives config pending and effective events, as for a local node.
- The hub then reaches algod's REST port through `ssh -W 127.0.0.1:<port> host`, which connects the ssh process's stdin and stdout to that port.
- algod speaks HTTP/1.1, which allows one request at a time per connection. Each node uses two `ssh -W` processes: one for the round watcher's long-poll and one for every other request. OpenSSH `ControlMaster`, where available, shares one TCP connection between them.
- The hub re-reads these files after a connection failure and every 10 minutes, so it follows a port change after an algod restart.
- This mode needs `cat` and a POSIX shell, so it supports Linux and macOS remotes. Windows remotes use the agent.

### SSH agent

A remote host runs `algod.top agent` as a service. The agent is the daemon minus rollups and the web UI:

- It runs the collectors, a local buffer (the same WAL and raw blocks, 7 days by default) and its own health rules and alerts.
- It has its own config file listing its local nodes, usually written by discovery during install.
- One host can carry several nodes. The hub names them `<remote>/<node>`, for example `relay1/mainnet`.
- Its alert rules and channels come from the hub's config. `algod.top remote install` copies them into the agent config, and `algod.top remote sync <host>` pushes later changes. Both run over SSH as an operator command, never through the web UI or API, so channels stay config-file only (section 11).
- The hub runs the agent by the absolute path recorded at install, for example `~/.local/bin/algodtop`. Non-interactive SSH sessions often skip the shell profile, so `PATH` cannot be relied on. The path is a per-remote config value.

The hub pulls with `ssh host algod.top stdio --agent`:

- `algod.top stdio` connects its stdin and stdout to the local daemon or agent socket. SSH handles authentication and encryption. The remote opens no port and needs no certificate or token.
- With `--agent`, if no agent service is installed, `stdio` starts an agent for the life of the connection. The hub then gets logs, process stats and host stats, but has gaps while it is offline.
- When an agent service is installed, `stdio` tries its socket first and never starts a second agent. A restarting service, such as one that has just upgraded itself, can take the better part of a minute to open its store, so `stdio` waits up to 2 minutes for it. A temporary agent started in that gap would hold the service's socket or store, or run as the wrong account beside a system agent.
- For a system agent the hub runs `stdio --agent --no-start --socket <the service's socket>`. That reaches the service directly, and a relay of any version accepts these flags, so the root-owned relay a system install left on the host need never change.
- The hub speaks HTTP/2 without TLS over the pipe (`Protocols.SetUnencryptedHTTP2`, Go 1.24 and later). `stdio` is a plain byte relay, so the agent and daemon sockets accept HTTP/2 with prior knowledge as well as HTTP/1.1. The batch pulls, live status and any terminal UI requests all share the one ssh process per host.

The pull protocol is the same over SSH and TLS:

- `GET /agent/v1/batches?since=<cursor>&wait=30s` returns compressed batches of samples, events, sessions, peer usage and kept raw log lines. With `wait`, the request returns as soon as new data exists, so live views on the hub stay within a second or two of the agent.
- The cursor makes pulls resumable. If the hub is down for a day, it catches up from the agent's buffer and no data is lost. The catch-up data advances that node's head and watermark in order (section 8), so it is not treated as late.
- The agent deletes pulled data once it is older than the buffer window (7 days by default). It keeps unpulled data past that window, up to a size cap (default 1 GB) or its own disk floor. Beyond either limit it deletes the oldest unpulled data first and records a retention deletion event, as the disk floor does (section 18).
- The protocol is versioned in the path. A hub supports the current and the previous agent protocol version, so hosts can be upgraded one at a time.

Alerts for an agent's nodes run on the agent, so a hub on a sleeping laptop does not silence them. The hub shows the agent's alert events, and fires only one alert of its own for those nodes: "agent unreachable". Hub-local, REST and no-install nodes alert from the hub. Each alert therefore fires from exactly one place.

### TLS agent

For hosts the hub cannot reach over SSH, the agent can listen on TCP:

- Agents accept only authenticated hub requests. Agents behind NAT need a tunnel or VPN to be reachable.
- Hub-to-agent traffic uses TLS with a shared token, or mutual TLS with certificates the hub generates via stdlib `crypto/x509`.

### Installing an agent

`algod.top remote install <host>` sets up an SSH agent. It is opt-in and changes nothing on the host without showing the plan first:

1. It detects the remote OS and architecture over SSH.
2. It downloads the matching release from GitHub on the hub, so the remote needs no internet access. It verifies the release's checksums file against an ed25519 signature, with the public key built into algod.top (stdlib `crypto/ed25519`).
3. It copies the binary through the ssh pipe into the user's home, so no `scp` is needed. Unix remotes get `~/.local/bin/algod.top` through `cat`. Windows remotes get `%LOCALAPPDATA%\algodtop\algod.top.exe` through PowerShell, because the default Windows OpenSSH shell is `cmd`.
4. It runs discovery on the remote and shows the agent config it would write, including the alert rules and channels copied from the hub.
5. It installs a user-level service where the platform allows: a launchd user agent on macOS, or a systemd user unit on Linux, which needs lingering enabled to run without a login session. On Windows it installs a Windows service (section 16) by running `algod.top service install --agent` over SSH once confirmed. That needs an administrator, and Windows OpenSSH runs an administrator's session elevated, so no `su` or `sudo` is involved. Remote commands quote a Windows path with double quotes for `cmd`, the default Windows OpenSSH shell.

The agent must run as the account that owns the algod data directory. Package installs run algod as a system user such as `algorand` under `/var/lib/algorand`. A service for that account is a system agent (section 16), and installing one needs root once. algod.top never becomes root without showing the command and asking:

- It asks how to become root on the host, `su` or `sudo`, and saves the answer as the remote's `root`. Some operators have no sudo rights and use `su` with the root password.
- It shows the command, such as `ssh -t <host> 'su -c "$HOME/.local/bin/algod.top service install --system --agent"'`, and runs it over `ssh -t` once confirmed, so `su` or `sudo` asks for the password in the terminal. The single quotes keep the hub's shell from expanding anything, and the host's login shell expands `$HOME` before root's shell starts.
- Then it runs `remote add` to record the agent. Without a terminal it prints the command for the operator to run instead.

`algod.top remote add <host>` points the hub at an agent that already runs, however it was installed. It tries `/usr/local/bin/algod.top`, `/usr/local/bin/algodtop`, `~/.local/bin/algod.top` and `~/.local/bin/algodtop` in turn unless `--path` names one, and runs `stdio --agent --no-start` so that a missing agent fails rather than starting one for the probe. It shows the agent's nodes and saves the remote with the agent's config path. It offers to replace nodes that watch the same host over SSH with nothing installed, since those would show twice.

The hub names an agent's node `<remote>/<node>` unless the remote's `names` maps it to another name. When `remote add` replaces an SSH node whose data directory matches one of the agent's nodes, it maps that node to the SSH node's name, so the history continues under one name. `--node-name` sets the name for an agent with one node, for remotes added before this existed. When a remote's names change, the hub pulls the agent's whole buffer again, so data already pulled under the old name is stored again under the new one. The cursor file records the names it was written under, which is how the hub notices. During that re-pull it skips items for nodes whose name did not change, up to the old cursor, so their events and sessions are not stored twice. Repeated samples are harmless, because raw merges keep one sample per timestamp and late windows rebuild their rollups from raw.

`algod.top remote upgrade <host>`, `algod.top remote sync <host>` and `algod.top remote remove <host>` cover the rest of the lifecycle. Each finds the saved remote by its SSH host as well as by `--name`, so `remote upgrade node1` finds a remote saved as `partmon1`. For a system agent, whose config path is under `/etc`, the SSH user cannot write the config. `sync` prints the alert settings to merge on the host, and `remove` drops the remote from the hub and prints the uninstall command, with the remote's `su` or `sudo`.

### Upgrades

`algod.top upgrade`, run on the hub, installs a release everywhere: on the hub's machine, then on every agent in its config. The newest release is the default, and `--version` names another. Each daemon and agent installs the release itself:

1. The hub downloads the release for each platform once, and verifies it.
2. It sends the checksums file, its signature and the binary to the daemon or agent as a form posted to `/api/admin/upgrade` on its socket. A remote agent's socket is reached through the SSH connection the hub already uses, so the remote needs no internet access and the agent no new port. The endpoint exists on the unix socket only. Over TCP it answers 404.
3. The daemon or agent trusts none of the hub's checks. It verifies the signature with its own built-in key and checks that the file is for its own platform. It writes the binary beside its own and runs it with `version --json`. That reads the version, which the signature does not cover, and shows that the binary runs on this machine.
4. It refuses a release that is not newer than its own build, by the commit time in the pseudo-version. Whoever can reach the socket can therefore only move algod.top forward to a genuine release, never back to an older one with a known bug. A development build takes any signed release.
5. It renames the new binary over its own, stops as for SIGTERM, and execs the new binary in the same process. systemd and launchd keep tracking the same PID, and the agent's buffer covers the gap, so the hub loses no data. Windows cannot exec, and cannot rename over a running binary but can rename it aside. A Windows service therefore moves its old binary aside, stops with an error code, and its recovery action restarts it on the new one 2 seconds later. It deletes the old copy when it starts. Run in a terminal on Windows, the new binary runs from the next start.
6. The hub waits for each one to answer with the new version, then reports it.

Access to the socket is the permission to upgrade. For a system agent that is the `algodtop` group, which the hub's SSH user joins at install. A member can already read the algod tokens through the socket, a bigger power than moving algod.top to a newer signed release.

A system agent can only replace a binary its account owns, so a system service runs its own copy in its store, such as `/var/lib/algodtop-agent/bin/algod.top`, owned by the service account (section 16). Agents installed before this run the root-owned `/usr/local/bin` copy and refuse with `not-writable`. Agents older still answer 404. For either, `upgrade` makes a one-time switch: it copies the release to the SSH user's `~/.local/bin/algod.top` and runs the system install as root, with the remote's `su` or `sudo`, as for a new install. From then on upgrades need no root. A user agent from before upgrades the old way: the hub copies the binary over SSH and restarts its service.

The hub's daemon looks for a newer release a minute after it starts and then daily, by following the redirect from GitHub's `releases/latest` to the release tag. A newer one shows in the status line of both UIs and on the Settings page, which names `algod.top upgrade`. `"update_check": false` turns the check off. The check only reports: upgrades are always run by an operator, because every merge to main is a release, and an unattended upgrade would carry a bad one to every host.

The hub runs agent pulls without an OpenSSH ControlMaster. A pull is one long-lived ssh process, so a master saves nothing. A shared master would also keep the login's groups from before the user joined `algodtop`, and the hub's retries would keep that master alive.

### Remote front ends

`algod.top -H ssh://hub` opens the terminal UI against the hub on the host `hub` through `ssh hub algod.top stdio`. A laptop can view an always-on hub at home with no open ports. `-H https://host:8090` with a read-only token works where SSH does not.

### Clocks and cost

The hub records clock offset per agent and stores agent timestamps as given, with the offset visible in both front ends.

Each remote host costs one `ssh` process on the hub, about 5 MB each, outside the daemon's budget in section 16. A no-install node uses two. Storage on the hub per remote node is the same as for a local node.

## 13. Terminal UI

`algod.top` with no subcommand opens the terminal UI. It behaves like `top`: one screen, live updates, single-key commands, and nothing to configure before it works.

### Main screen

```
algod.top  host algod-host  10:52:14  daemon up 3d 4h  store 2.1 GB  nodes 5: 4 OK, 1 Warn
mainnet  round 52184330  rtime 2.79s  tpb 41  pool 312     range 1h  live

NODE       NET       VER       ROUND     LAG   RTIME  WS i/o  P2P i/o  CPU%  RSS   NET KB/s  HEALTH
mainnet    mainnet   4.4.1     52184330  0.6s  2.79s  12/4    38/4      6.1  2.1G  410/380   OK
testnet    testnet   4.4.1     61022817  0.4s  2.80s  8/4     11/4      2.3  1.4G  95/90     OK
localnet   localnet  5.1.0     9330871   0.5s  2.01s  2/4     6/4       1.9  1.1G  40/38     OK
mainnet-2  mainnet   4.4.0     52184329  0.8s  2.79s  4/4     0/0       1.2  0.9G  30/28     WARN key expires 6d
betanet    betanet   -         -         -     -      -       -         -     -     -         DOWN

EVENTS
10:47:02  mainnet    disconnect        in  p2p  1.2.3.4  reason=reset
10:31:40  mainnet-2  warn              participation key for ABC... expires in 6 days
```

The first two lines summarize the host and the selected node, like the load and task lines in `top`. The table has one row per node. The bottom pane tails recent events.

### Node screen

Enter on a node opens its detail screen:

- A header with round, lag, version, uptime and the last effective config change.
- Panes for consensus, network, bandwidth and resources. Each line shows a metric's current value and a sparkline over the selected range, one bucket per terminal column.
- Below them, a pane selected by key: peers, events or log templates. A list longer than the screen scrolls, and a footer such as `rows 21-40 of 120` says which rows show.

The peers pane works like the process list in `top`. It lists address, direction, network, time connected, resets in the last 24 h and cluster membership, sortable by any column. With per-peer usage it adds receive and send rates, session totals and open streams. Sorting by rate gives a live top-talkers list.

### Keys

| Key | Action |
|---|---|
| Up, Down, Enter, Esc | Select a node, open it, go back |
| PgUp, PgDn, Home, End | On a node, scroll the open pane. Up and Down scroll it too |
| `<`, `>` | Change the sort column |
| `p`, `e`, `l` | Peers, events or log templates pane |
| `r` | Cycle the range: 1h, 6h, 24h, 7d, 30d, 1y, all |
| `f` | Toggle full resolution (`tier=raw`) |
| `[`, `]` | Move the time cursor back or forward one bucket |
| `t` | Jump to a time, for example `2026-11-03 09:30` |
| Space | Pause or resume live updates |
| `a` | Add an annotation at the cursor |
| `s` | Add a silence for the selected node |
| `/` | Filter rows |
| `W` | Write discovered nodes to a config file, after showing it |
| `?` | Help |
| `q` | Quit |

The time cursor turns the screen into a view of the past. With the cursor at 09:30, every value, table and pane shows the state at that moment, and the header reads "at 2026-11-03 09:30:00" instead of "live". This is how an operator reviews history from a shell.

### Batch mode

`algod.top -b` prints the node table once and exits, like `top -b`. `-n N -i SECS` repeats it, and `--json` prints JSON for scripts. This works over SSH and in cron without a terminal.

### Startup

`algod.top` and `go run algod.top@latest` run the same code and start the same way. Each step runs only if the earlier steps found nothing. A flag that names a source skips straight to it.

1. Daemon. Connect to the daemon at `--socket` or `--hub`, else at the default socket path. A running daemon gives the full history, and the terminal UI uses it as is.
2. Config file. Read `--config`, else `$ALGODTOP_CONFIG`, else the platform default path (section 16). With no daemon, the terminal UI polls the config's nodes directly in live-only mode.
3. Explicit nodes. `-d` data directories and `-u` REST URLs from the command line.
4. Discovery. Check `$ALGORAND_DATA`, then running algod processes, then the platform's usual data directory locations (section 4).
5. Nothing found. Show a help screen with the flags and an example, rather than an empty table.

In live-only mode the terminal UI polls algod directly with the same collector code as the daemon. It shows a banner "no daemon: live only, no history". It keeps nothing on disk, so it is safe to run on any host.

Finding running algod processes works on every platform without privileges:

- List processes named `algod`: `/proc/*/cmdline` on Linux, `ps -axo pid=,args=` on macOS, and a Toolhelp32 snapshot on Windows.
- Take the `-d` argument where the command line is readable.
- Match PIDs against `algod.pid` in candidate directories. A directory whose `algod.pid` names a live algod process is a running node, even when algod was started through `$ALGORAND_DATA` rather than `-d`.
- A data directory with no live process is shown as DOWN.

Discovered nodes exist only for that session. Pressing `W` writes them to a config file after showing the path and contents, like `W` in `top`. The daemon never uses discovery on its own. `algod.top daemon --discover` prints what it found and writes it to the config, and running that command counts as the user's confirmation.

### Flags

| Flag | Applies to | Meaning |
|---|---|---|
| `-c, --config PATH` | all | Config file |
| `-d, --datadir [NAME=]DIR` | all | A local algod data directory. Repeatable. The same letter as `goal` and `algod`. |
| `-u, --url [NAME=]URL` | all | A remote node: an algod REST URL, or an `ssh://host/<datadir>` URL (section 12). Repeatable. |
| `--token-file PATH`, `--admin-token-file PATH` | all | Tokens for `-u` REST nodes. Nodes that need different tokens go in a config file. |
| `-s, --socket PATH` | TUI, daemon | Daemon socket to connect to, or for the daemon to create |
| `-H, --hub URL` | TUI | A remote daemon, as `ssh://host` or `https://host:port` (section 12) |
| `-l, --listen ADDR` | daemon | Web UI listener. An empty value disables it. |
| `--store-dir DIR` | daemon, agent | algod.top's own data directory (section 16). Named apart from `-d`, which is always an algod data directory. |
| `--live` | TUI | Ignore any daemon and poll algod directly |
| `--no-discover` | TUI | Skip step 4 |
| `-b`, `-n N`, `-i SECS` | TUI | Batch mode, iteration count, interval |
| `--json` | TUI | JSON output in batch mode |
| `--utc` | TUI | Show times in UTC |

Precedence is flags, then environment variables, then the config file, then discovery. Tokens are never accepted as flag values, because command lines are visible to every user in `ps`. The token sources are the token file flags, `$ALGOD_TOKEN` and `$ALGOD_ADMIN_TOKEN`, the config file, and the files in a local data directory.

`-d` means data directory, as it does for `goal` and `algod`, which is what algod operators expect. `top` uses `-d` for its delay, so algod.top uses `-i` for the interval instead.

### Implementation

The terminal UI uses `golang.org/x/term` (approved, section 15) and the stdlib:

- `term.MakeRaw` and `term.Restore` for raw mode on all three platforms. The terminal UI restores the saved mode on exit, on interrupt and on panic.
- `term.GetSize` for the window size. Unix re-reads it on SIGWINCH. Windows has no SIGWINCH, so the terminal UI checks the size before each redraw.
- ANSI escape sequences for cursor movement, colour and the alternate screen buffer. Windows Terminal handles these natively. The older console host needs `ENABLE_VIRTUAL_TERMINAL_PROCESSING`, which the terminal UI sets through `golang.org/x/sys/windows` at startup.
- Redraws only changed lines, so it stays cheap over a slow SSH link.
- Respects `NO_COLOR`, and falls back to ASCII sparklines when the terminal cannot show Unicode block characters.

The terminal UI talks to the daemon over the unix socket with the same API as the web UI. It runs as its own process, so its rendering cost never lands on the daemon.

## 14. Web UI

### Principles

- The first screen shows status. The Fleet page answers "is anything wrong" without scrolling.
- Every chart has units, a y-axis that starts at zero for rates and counts, and a title that is a question or a plain noun ("Inbound P2P connections"). No chart is unlabelled.
- Time is shared across charts. Hover shows a crosshair and values on every chart on the page at the same instant. Dragging on any chart zooms all of them. This synchronized crosshair is the netdata feature most worth keeping.
- Events and annotations draw as thin vertical markers on every chart, with a labelled event lane above the first chart. Hovering a marker shows the event. Clicking it filters the events table. Peer connects and disconnects, log warnings and an alert's hourly reminders are too frequent for markers; they stay in their own charts and tables.
- An events request returns the newest 5000 events and a count of every kind in its range. Peer churn on a busy relay can fill that cap in an hour or two, so a page whose answer comes back short asks again for the rarer kinds on their own.
- Gaps show as gaps. A line is never drawn across missing data, and a hatched region marks "algod.top was down" or "algod was down" separately.
- The URL holds all view state (node, range, panels, comparison, full resolution), so any view can be bookmarked or pasted into a chat.
- The UI follows the OS dark/light setting and has a toggle. The palette is colour-blind safe, with fixed colours per meaning used everywhere: inbound is one hue, outbound another, ws vs p2p by lightness. The terminal UI uses the same mapping.
- The UI works at phone width for a quick check, and is designed for a desktop.
- Interaction stays under 100 ms for a 2000-point chart. The UI uses SVG for line, area and bar charts, with one path per series rather than one element per point, and canvas for heatmaps.

### Pages

1. Fleet. One card per node shows:
   - the health state and its reason
   - network, version, round and lag
   - peers as in/out for ws and p2p, or n/a when the node has no admin token. The counts come from `/v2/node/peers`, so without the token they would read 0.
   - a 24 h sparkline each for bandwidth and round time
   - participation status
   - the last notable event

   A sortable table view shows the same data.
2. Node. A header strip shows live round, lag, version, uptime and the last effective config change. Below it are the time-range controls: 1h, 6h, 24h, 7d, 30d, 1y, all, custom, "since last restart", and the full-resolution toggle. Collapsible sections of synchronized charts follow:
   - Consensus: round time, votes and proposals, transactions per block, pool size
   - Network WS: peers, bytes by direction, disconnect reasons as stacked bars
   - Network P2P: connections, gossip streams, pubsub peers, DHT, evictions, setup timeouts, resource manager blocks
   - Bandwidth: the views from section 10
   - Resources: CPU, RSS, FDs, disk I/O, disk free, data dir size
   - Logs: template counts as a heatmap over time, plus warnings and errors
3. Peers. The current peers table comes from `/v2/node/peers`. The history table has one row per remote address:
   - sessions and total time connected
   - disconnect reasons
   - reset count
   - which clusters the address was part of
   - bytes received and sent per session, with per-peer usage

   With per-peer usage, the current peers table adds peer ID, rates, open streams and supported protocols. The clusters list shows each cluster's timeline.
4. Events. This is a filterable table of events, annotations and silences, with a timeline strip. The user can add an annotation from here, or by selecting a range on any chart.
5. Compare. It overlays two time ranges for one node, aligned to their start, such as run 2 vs run 3. It can also overlay two nodes over the same range. For each metric it shows a summary table of mean, p50, p99, and the difference.
6. Explorer. The user picks any stored metric and label filter and gets a chart. It covers questions nobody anticipated, without code changes.
7. Settings. Nodes, retention and auth, read-only by default. Edits require login and write the config file atomically. Alert channels are never editable here (section 11).

### Chart types and when each is used

| Type | Used for |
|---|---|
| Line with min/max band | Gauges over long ranges (RSS, peers, lag) |
| Step line | Small integer gauges (peer counts, gossip streams) |
| Stacked area | Bandwidth by component, peers by type |
| Stacked bar per bucket | Event counts (disconnect reasons per 10 min) |
| Heatmap | Histograms over time (dial latency, DHT RPC latency), log template counts, round time by hour of day |
| Sparkline | Fleet cards and table cells |
| Single value with delta | Header strip (round, lag, peers), with change vs 24 h ago |
| Table with inline bars | Peers history, template counts, compare summary |

### Implementation

The UI is plain ES modules, embedded into the binary with `go:embed`, with no build step. One file per chart type (`line.js`, `heatmap.js`, ...) shares an axis, scale and tick module. The code needs:

- time and number scale functions
- "nice" tick generation
- SI and binary unit formatting
- LTTB downsampling as a fallback when a raw-tier request still has too many points
- a crosshair bus that synchronizes hover across charts

It uses system fonts only. Nothing loads from a CDN, so the UI works offline and leaks nothing.

## 15. Dependency policy

The default is the Go standard library plus the approved dependencies below. The project uses Go 1.27: `go.mod` sets `go 1.27.0` and pins the toolchain to go1.27.3. The features algod.top relies on most (`crypto/pbkdf2`, `os.Root` and unencrypted HTTP/2) arrived in Go 1.24.

Approved dependencies:

| Dependency | Used for |
|---|---|
| `golang.org/x/term` | Raw mode and window size for the terminal UI on Linux, macOS and Windows |

`golang.org/x/term` depends on `golang.org/x/sys`, so `x/sys` is in the module graph already. Using its packages directly adds no new module, but each use is still listed here:

| Package | Would replace | Recommendation |
|---|---|---|
| `golang.org/x/sys/windows` | `syscall.NewLazyDLL` lookups for the Win32 process, network and console calls | Use it. It gives typed wrappers for the same calls. |
| `golang.org/x/sys/windows/svc` | A wrapper program such as WinSW to run the daemon as a Windows service | Use it. The stdlib cannot answer the Windows service control manager. |
| `golang.org/x/sys/unix` | Exec of `ps` on macOS for process stats | Verify first. Its sysctl `kinfo_proc` path may not expose RSS, CPU time or FD counts, which come from `proc_pidinfo` in libproc. |

Other candidates. Each is listed for approval, and none is planned:

| Candidate | Would replace | Recommendation |
|---|---|---|
| `golang.org/x/crypto/argon2` or `bcrypt` | PBKDF2-SHA256 password hashing | Not needed. PBKDF2 with 600k iterations is acceptable for a single admin password. |
| `golang.org/x/crypto/ssh` | The system `ssh` binary for remote hosts | Not needed. The system client honours `~/.ssh/config`, ProxyJump and ssh-agent, which the library would not (section 12). |
| `modernc.org/sqlite` | Custom events and sessions store | Fallback only, see section 8. |
| `golang.org/x/net/websocket` | SSE | Not needed. |

## 16. Daemon, config and deployment

### Commands

| Command | Does |
|---|---|
| `algod.top` | Terminal UI (section 13) |
| `algod.top daemon` | Collector, store, health, alerts and API, with the web UI when `web.listen` is set |
| `algod.top agent` | Collector and local buffer for a remote host (section 12) |
| `algod.top export` | Exports series, events or sessions |
| `algod.top silence` | Adds, lists or removes silences |
| `algod.top stdio` | Connects stdin and stdout to the local daemon or agent socket, for use over SSH. `--agent` starts an agent for the connection if none is running. |
| `algod.top remote` | `install`, `add`, `upgrade`, `sync` or `remove` an agent on a host over SSH (section 12) |
| `algod.top upgrade` | Install a release here and on every remote agent (section 12) |
| `algod.top service` | Explains, installs, removes or reports the service that starts the daemon at login or boot (section 16) |
| `algod.top reload` | Asks the daemon to reload its config, the same as SIGHUP. Windows has no SIGHUP, so this is the reload method there. |
| `algod.top passwd` | Hashes a password into the config |
| `algod.top check [--notify]` | Validates config and alert rules, reaches each node, prints projected disk use and any pending retention deletion. `--notify` sends a test notification on each channel |
| `algod.top version` | Prints the version |

### Config

- The config is one JSON file (stdlib `encoding/json`). The file contains tokens and the password hash. On Linux and macOS, algod.top warns if the file is group- or world-readable. On Windows, it warns if the file's ACL grants read access beyond the user, SYSTEM and Administrators.
- `SIGHUP` or `algod.top reload` reloads nodes, alerts and retention. A listen address change needs a restart.
- Listen settings are `web.listen` (default `127.0.0.1:8090`, empty to disable), optional `tls_cert`/`tls_key`, or `tls: "self-signed"` to generate and persist a certificate.
- Paths in the config accept forward slashes on every platform.

| Platform | Config file | Data directory |
|---|---|---|
| Linux | `$XDG_CONFIG_HOME/algodtop/config.json`, default `~/.config/algodtop/config.json` | `$XDG_DATA_HOME/algodtop`, default `~/.local/share/algodtop` |
| macOS | `~/.config/algodtop/config.json` | `~/Library/Application Support/algodtop` |
| Windows | `%APPDATA%\algodtop\config.json` | `%LOCALAPPDATA%\algodtop` |

### Resource budget

The daemon may share a machine with several algod processes and must not compete with them. With five Mainnet-sized nodes and no viewers connected, the targets are:

- under 0.5% of one core on average
- under 64 MB RSS
- under 50 KB/s average disk writes

The daemon reports its own usage as series and on the terminal and web UIs. The prototype measures all three in milestone 1.

The daemon meets these targets with these measures:

- Heads stay compressed in memory, about 1.2 MB per node.
- `GOMEMLIMIT` defaults to 96 MiB and `GOMAXPROCS` to 2. Both are configurable.
- The log prefilter skips JSON decoding for trace lines, which are most of the volume.
- Rollups come from the in-memory head at flush time, so they cost no extra reads.
- Background jobs (compaction, scrub, backfill, deletion) run one at a time, under an I/O limit of 10 MB/s by default.
- The daemon runs at a lower priority. Under systemd that is `Nice=10` and `IOSchedulingClass=best-effort` with `IOSchedulingPriority=7`. Under launchd it is `Nice` with `LowPriorityIO`. On Windows the daemon sets `BELOW_NORMAL_PRIORITY_CLASS` on itself at startup.
- The daemon does no rendering and runs queries only when a client asks.

### Service and lifecycle

- `algod.top service` explains how to run the daemon as a service on the current machine and prints the file it would write. `algod.top service install` writes the file, enables it and starts it, and `uninstall` and `status` cover the rest. Every service restarts the daemon after a failure and runs it at the low priority above. Running `algod.top daemon` by hand in a terminal or tmux needs none of this.
- On macOS it installs a launchd user agent in `~/Library/LaunchAgents/top.algod.daemon.plist`, which starts at login. launchd gives jobs a bare `PATH`, so the plist sets one that includes Homebrew and the binary's own directory for alert commands.
- On Linux with systemd it installs a user unit in `~/.config/systemd/user/algodtop.service`, which starts at login, or at boot once lingering is enabled. When the node data belongs to another account, as with a package install where algod runs as `algorand` from `/var/lib/algorand`, it recommends a system service instead. `sudo algod.top service install --system` then mirrors `algorand.service`: it runs as the data directory owner from boot, reads `/etc/algodtop/config.json` (written from discovery when missing, and owned by that account so web UI edits can save) and stores data in `/var/lib/algodtop` through `StateDirectory`. A system unit must not run a binary from a home directory, so the install copies itself to `/usr/local/bin/algod.top` for people to run, and replaces an old `/usr/local/bin/algodtop` with a link to it. The service itself runs another copy in its store, `bin/algod.top`, owned by the account it runs as, so the service can replace it when `algod.top upgrade` asks (section 12). The store's snapshots and size leave `bin` out. Before it starts the service, the install runs that copy as the service account; on a store mounted `noexec` that fails, and the service runs the `/usr/local/bin` copy instead, which only root can upgrade.
- `sudo algod.top service install --system --agent` installs the agent the same way, as `algodtop-agent.service` with `/etc/algodtop/agent.json` and `/var/lib/algodtop-agent`. This is how a hub pulls from a package install (section 12).
- A system service runs with `Group=algodtop` and `--socket-group algodtop`, so its state directory and its socket (mode 0660) belong to the `algodtop` group. The install creates the group and adds the person behind `sudo`. Members can use the daemon's terminal UI and CLI commands without sudo, and the hub can pull from the agent over SSH as one of them. Membership grants the socket's write role. Alert channels stay config-file only (section 11), so membership does not let anyone run a program as the service account. The group applies to new logins only.
- CLI commands, the terminal UI and `stdio` also try the system service's sockets, `/var/lib/algodtop/algodtop.sock` and `/var/lib/algodtop-agent/agent.sock`. When a socket refuses them, they say why: the account is not in the group, or joined after this login began. `stdio --agent` then fails rather than start a second agent beside the service.
- The install refuses when a daemon already answers on the service's socket and the service manager is not running it, since both would use one store. Elsewhere, including Linux without systemd, `algod.top service` prints what to run instead, such as a cron `@reboot` line. On Windows, see the Windows service below.
- The daemon runs as the account that owns the algod data directories, because it needs to read the token files. On Linux and macOS it never runs as root.
- On Windows, `algod.top service install [--agent]`, from an elevated prompt, registers a Windows service through `golang.org/x/sys/windows/svc/mgr`: `algodtop` for the daemon and `algodtop-agent` for the agent, started at boot. It runs as LocalSystem. FUNC runs algod under a per-service virtual account, such as `NT SERVICE\Algorand Node`, and only LocalSystem or an administrator may read that process's figures and command line; a user account would also need its password stored. Everything the service owns sits in one directory, `%ProgramData%\algodtop` or `%ProgramData%\algodtop-agent`, which the install restricts to SYSTEM and Administrators: the config (`config.json` or `agent.json`), the socket, `store\`, its own copy in `bin\algod.top.exe`, and `log\algod.top.log`, which it writes because a service has no console. Only SYSTEM and Administrators can open the socket, so on Windows the Administrators group plays the part of the `algodtop` group, and the terminal UI and CLI run from an elevated prompt.
- Its recovery actions restart it 2, 5 and 30 seconds after a failure, including a stop with an error code, which is how an upgrade restarts it (section 12). Installing again stops it, replaces its copy and updates its settings. `uninstall` removes the service and its copy and keeps the config and store, and `status` asks the service control manager and the socket.
- Shutdown flushes each head block and its partial rollups, fsyncs, and exits within 10 s. SIGTERM triggers it on Unix. On Windows, a service stop request or Ctrl+C triggers it.

### Distribution

The Go module path is `algod.top`, and so is the program's name:

| Command | Builds from | Binary name |
|---|---|---|
| `go install algod.top@latest` | repo root | `algod.top` |
| `go run algod.top@latest` | repo root | none, runs and exits |

`go install` names a binary after the last element of its import path, so the module path gives the command its name, `algod.top`, or `algod.top.exe` on Windows.

- The docs, the site, help text, both UIs and the release files (`algod.top_linux_amd64`) use `algod.top`. Help and error text print the name the binary was run under.
- Names that machines read keep `algodtop`, where a dot is not allowed or a new name would strand existing data: environment variables (`ALGODTOP_CONFIG`), metric names (`algodtop_round`), config and data directories, systemd units, the `algodtop` group, socket names, and the User-Agent algod logs.
- The program was first named `algodtop`. A second five-line `main` package keeps `go install algod.top/algodtop@latest` building it under that name, so earlier installs still update, but the docs do not show it. A system service install replaces an old `/usr/local/bin/algodtop` with a link to `/usr/local/bin/algod.top`.

algod.top has no release numbers. Every commit on main is a release, and Go names it with a pseudo-version that holds the commit's UTC time, such as `v0.0.0-20261104123549-37f0e9db466c`. `go install algod.top@latest` builds the newest commit on main:

- `algod.top version`, the Settings page and `remote add` show the commit's time and age, such as `2026-11-04 12:35 UTC (37f0e9db), 3 days old`. A local build reads them from Go's VCS stamp, and an installed one from its pseudo-version.
- The repo has no semver tags. A v1 tag would make `@latest` follow tags rather than main, and v2 or later would move the install path to `algod.top/v2`.
- Changes reach main through a pull request once CI passes, so every release has passed CI.

`go run algod.top@latest` is the zero-install entry point. It follows the startup order in section 13: a running daemon if one exists, then a config file, then flags, then discovered local nodes. Flags pass through as usual, for example `go run algod.top@latest -d ~/.mainnet` or `go run algod.top@latest daemon -l :8090`.

`https://algod.top/?go-get=1` and `https://algod.top/algodtop?go-get=1` serve a `go-import` meta tag for `algod.top` pointing at the GitHub repo. The module path survives a repo move. The domain must stay registered for as long as the module exists. The Go checksum database protects published versions if it ever lapses, but new versions would be lost.

`algod.top` also hosts the docs and the block format spec.

Every merge to main also publishes a GitHub release, after the tests pass on all three platforms. It holds a binary per platform (Linux, macOS and Windows on amd64 and arm64, and FreeBSD on amd64), `checksums.txt` with the SHA-256 of each, `checksums.txt.sig` with the ed25519 signature of that file, and a GitHub build attestation per binary. The release is named by its commit as Go names it, and its tag is the pseudo-version without `v0.0.0-`, such as `20261104123549-37f0e9db466c`, so no semver tag ever appears. `algod.top upgrade` installs the newest release (section 12), and `algod.top remote install` fetches the release that matches the hub's own version, or `--version latest`. Releases from before the rename name their files `algodtop_<os>_<arch>`, and algod.top still reads them.

The release key's public half is in `internal/release`, so every build can verify a download. The private half is the `ALGODTOP_RELEASE_KEY` secret of the repository's `release` environment, which only main can use, and the owner keeps an offline copy. The release job checks its signature against the public key before it publishes. Replacing the key means a new key pair, a commit with the new public half and a new secret. Binaries built before that keep the old public key, so they can no longer verify new releases and need one manual update.

## 17. Security

- The unix socket is restricted to the daemon's user (section 3). Anyone who can open it already runs as the user who can read the algod tokens, so it grants nothing new. A system service also opens it to the `algodtop` group (section 16). Members can change silences and, with `web.allow_config_edits`, nodes and retention, but never alert channels. An ssh URL whose host or user starts with `-` is rejected, because ssh would read it as an option such as `-oProxyCommand`.
- The TCP listener binds to localhost by default. When the listen address is not loopback and no password is set, the daemon refuses to start unless `allow_unauthenticated_remote: true` is set, and it logs a warning.
- Auth on TCP is optional. Passwords are hashed with PBKDF2-SHA256 and a per-password salt. Sessions use a random 256-bit token in an `HttpOnly`, `SameSite=Strict` cookie, `Secure` under TLS, with a configurable expiry. Logins are rate-limited per IP and failures are logged.
- Read-only access can be given separately. An optional read-only password or bearer token allows a fleet page on another screen, or a remote terminal UI.
- Every state-changing request over TCP (annotations, silences, settings, snapshot) needs login and a CSRF token.
- Alert channels, webhook URLs and commands are config-file only (section 11). A stolen web password cannot make the daemon run a program.
- algod tokens never reach a client. The admin token is used only for the GET endpoints listed in section 5, and a code-level allowlist of algod paths enforces it. A test asserts that no other admin path is called.
- algod.top stores peer IP addresses. The default retention for peer-session raw addresses is 1 year. After that, algod.top keeps only aggregates (counts per /24 or per ASN if a local ASN file is supplied).
- For access away from home, the docs recommend `algod.top -H ssh://host` or a VPN such as Tailscale over exposing the port. The SSH transport needs no open port at all.
- Over SSH, the SSH login is the access control. Anyone who can log in as the remote user can already read that user's algod tokens, so `algod.top stdio` grants nothing new. The hub always runs `ssh` with `BatchMode=yes`, so it never accepts an unknown host key.
- Tokens read over SSH for no-install nodes stay in the hub's memory and are never written to disk.
- `algod.top remote install` only installs binaries whose checksums file carries a valid ed25519 signature from the algod.top release key.

## 18. Running indefinitely

- algod changes. Metric names change, log messages change, and new fields appear. The alias table and per-extractor fixtures catch this. A "parser health" panel shows unparsed-line rates and unknown metric names after each algod upgrade, so breakage is visible rather than silent.
- algod.top changes. The store has a format version and migrations that run once on upgrade, with a backup taken first. Old blocks stay readable, and a new block format writes only new data.
- Time. algod.top stores UTC with nanosecond resolution and lets each front end choose local or UTC. It records host clock jumps (wall time going backwards) as events. A jump never reorders stored data. Samples older than the current head go through the late-data path (section 8).
- Disk. Retention windows make disk use predictable. The daemon measures bytes per stored value and projects total use for the configured windows. `algod.top check` prints the projection, and the daemon raises Warn if it exceeds free space minus the floor.
- Disk floor. When the volume holding the data directory drops below the floor (default 5% free), new data wins over old. An operator short on disk loses history but keeps collection, health checks and alerts. The daemon deletes the oldest data first, in this order, until free space is 1% above the floor:
  1. Optional data: raw Info lines and investigation captures.
  2. Raw blocks, down to a minimum of 2 days. Their rollups already exist, so the history keeps its shape at 1-minute resolution.
  3. 1m blocks, down to 30 days.
  4. 10m blocks, down to 1 year.
  5. Then the oldest block of any tier, so the newest data is always the last to go.

  Each pass records a retention deletion event naming the tiers and time ranges removed. The node health shows Warn with the shortened coverage in words, for example "Warn: disk floor, raw history is 12 days instead of 30". Health checks and alerts run from memory and keep working throughout.
- If deleting everything outside the heads still leaves the volume below the floor, the daemon pauses writes and raises Critical. Collection, health checks and alerts continue in memory, and the pause is recorded as a monitor gap. algod's disk takes priority over algod.top's data.
- An operator who prefers a strict retention guarantee sets `disk.at_floor: "pause"`. The daemon then pauses writes at the floor and never deletes data inside a window.
- The daemon warns when its data directory and an algod data directory share a volume that is under 15% free.
- Bit rot. Every block and record has a CRC32. A weekly scrub, under the background I/O limit, verifies them, marks bad blocks, and reports them in both front ends rather than failing queries.
- Testing at scale. A generator creates synthetic data for 5 nodes over a history of any chosen length in minutes, for benchmarks of rollup, query and render times.

## 19. Testing

- Parser tests use fixtures in the shape of real `/metrics` output and `node.log` lines, with accounts, IDs and addresses replaced, including flood and rotation cases. The `/metrics` fixtures include the flattened histogram families.
- The store gets Go native fuzzing of block and WAL decoding, property tests (written data equals read data at every tier), and crash tests that kill the writer at random points and check recovery.
- Retention tests advance a fake clock across a long simulated history and assert that no sample inside any tier's window is ever missing, including across crashes during rollup, compaction and deletion.
- Disk floor tests fill a small fake volume and check the deletion order, the minimum windows, the events and the health text. They also check that collection and alerts continue throughout.
- CI builds and runs the unit tests on Linux, macOS and Windows from milestone 1. The fake algod, the store and the tailer are platform-neutral, so most tests run everywhere. Each platform's process-stats code is tested against the test process itself, and the Windows tailer test checks that a rename succeeds while the tailer holds the file open.
- Late-data tests replay an agent outage and a clock jump, and check that queries and rollups match an in-order run.
- Per-peer usage tests use `/v2/node/peers` fixtures from both a current algod and a build that reports per-peer usage. They cover repeated p2p totals across connections, resets on reconnect, and responses without usage fields.
- SSH tests put a fake `ssh` executable first in `PATH`. It runs the requested remote command locally and pipes it back, so the SSH modes, reconnects and the `stdio` relay are tested without an SSH server.
- Collector tests run against an `httptest` fake algod that can stall, restart (counter reset), rotate logs, change ports and disable API auth.
- Terminal UI tests render screens into a fixed-size buffer and compare them with golden files.
- A small web UI smoke test serves a fixed dataset and checks that each page renders without console errors. Running it needs a headless browser on the developer machine. It is not a runtime dependency.
- A footprint test runs the daemon against five fake nodes for an hour and fails if CPU, RSS or disk writes exceed the budget in section 16.

## 20. Milestones

1. Capture and live view. Local nodes, remote nodes over REST and SSH without install, `/metrics`, the round watcher, process stats behind the platform interface, raw tier, WAL and heads, the unix socket API, the terminal UI node table and batch mode, service files, and a "down or stalled" alert through the command and webhook channels. CI on all three platforms. Measure real storage size and the daemon footprint here. This replaces ad hoc sampling and bandwidth scripts.
2. Retention and history. Partial rollups, compaction, retention by guaranteed window, tier stitching, the full-resolution toggle, terminal UI sparklines and time cursor, and export.
3. Logs and events. Tailer with rotation, templates, extractors, events, annotations, silences, and the terminal UI events and logs panes. Config pending and effective detection, and version changes.
4. Web UI. Fleet and Node pages, synchronized charts, the event lane and the Events page.
5. Peers. Sessions from logs and `/v2/node/peers`, per-peer usage when the node reports it, cluster detection, the terminal UI peers pane and the web Peers page. Milestones 3 and 5 together replace manual log greps.
6. Health and alerting. All health rules including participation, SMTP, repeat intervals and the heartbeat ping.
7. Auth and TLS. Passwords, sessions, CSRF, read-only tokens, TLS options and the remote terminal UI.
8. Compare and Explorer.
9. Agents. The agent, the `stdio` relay, the SSH and TLS agent modes, `algod.top -H ssh://`, and `algod.top remote install`.

Windows support is not a separate milestone. The platform interface, file sharing flags, socket, reload command and CI exist from milestone 1. The Windows process collectors, the service install and the discovery paths ship as soon as an official algod Windows binary exists to test them against, whichever milestone is in progress then.

Each milestone is usable on its own.

## 21. Upstream algod changes that would help

These go in go-algorand, not algod.top. Each is small.

- A libp2p total bytes counter in `/metrics`. It would close most of the gap between algod's counters and wire bytes without per-process OS tricks. The per-peer usage fields deliberately come with no host-wide total, because every stream write would contend on one counter. A total that needs no hot-path counter is possible: keep a closed-peer accumulator that the per-peer counter adds to when it drops a peer, and add the live per-peer totals at scrape time. The result only grows, and stream writes touch nothing new.
- Release the per-peer usage fields, which add peer ID, protocols, streams and approximate bytes to `/v2/node/peers`. Supported protocols would show whether peers that reset together run the same software, and a software version string in the same response would settle it.
- An Info-level log line for P2P inbound eviction with the peer address. Today it is Debug, which makes the eviction-vs-reset correlation depend on metrics alone.
- A transaction pool size gauge in `/metrics`, which would remove the pending-transactions poll.
- Proper `histogram` TYPE lines for histogram families, rather than one `counter` family per suffix.

## 22. Open decisions

- Default scrape interval: 10 s (the proposal) or 5 s. 5 s doubles the raw tier size.
- Whether to backfill existing log files on first start. Backfill gives instant history, but a busy Mainnet node's two files can total several GB.
- Whether the web UI should allow config edits in v1, or stay read-only with config edited by hand.
- Whether to keep all Info log lines raw for a short window (for example 3 days) by default. This costs disk and gives full post-mortems.
- Disk floor minimums: 2 days of raw, 30 days of 1m and 1 year of 10m before the floor deletes across tiers.
- Approval for `golang.org/x/sys/windows` and `golang.org/x/sys/windows/svc`. Both come from a module already in the graph through `golang.org/x/term`.
