# algodtop store format, version 1

This document describes every file in algodtop's data directory, so the data stays readable without algodtop. Integers are little endian unless marked varint. A varint is Go's `encoding/binary` signed varint (zigzag) and a uvarint is the unsigned form. CRC32 is the Castagnoli polynomial unless a section says IEEE.

`meta.json` holds the format version. A newer algodtop migrates once on upgrade, after taking a backup, and old blocks stay readable.

## Directory layout

```
<data>/
  meta.json                       format version, created time, node registry
  LOCK                            advisory lock held by the running daemon
  algodtop.sock                   API socket (mode 0600)
  wal/<first seq>.wal             write-ahead log segments, 64 MB each
  series/index.bin                series registry, append-only
  chunks/raw/YYYY-MM-DD/          2 h raw blocks
  chunks/1m/YYYY-MM/              1m rollups: 2 h partials, day blocks
  chunks/10m/YYYY/                10m rollups: 2 h partials, week blocks
  chunks/1h/YYYY/                 1h rollups: 2 h partials, month blocks
  events/YYYY-MM.log              events, length-prefixed JSON
  sessions/YYYY-MM.log            finished peer sessions
  logs/YYYY-MM-DD.log             kept raw log lines (warnings and errors)
  peers/YYYY-MM-DD/n<id>-<s>.pu   per-peer usage samples, 2 h per file
  annotations.json                annotations, rewritten atomically
  state/                          watermarks, counter state, alerts, silences, tail offsets
  snapshots/<time>/               backups made by POST /api/admin/snapshot
```

Windows align to 2 h UTC boundaries. Every 1m, 10m and 1h bucket falls inside exactly one window.

## meta.json

```json
{"format": 1, "created": 1793708935000000000,
 "nodes": [{"id": 1, "name": "mainnet", "genesis_id": "mainnet-v1.0", "genesis_hash": "wGHE2...", "created": 0, "retired": false}]}
```

A node is a name plus a genesis hash. When a name reappears with a different genesis hash, the old entry is marked retired and the name gets a new ID, so two networks never share one history. Block file names use the node ID.

## Block file names

`n<node>-<start unix seconds>.<kind><gen>.blk`, where kind is:

| Letter | Kind | Window |
|---|---|---|
| g | main raw block, flushed from a head | 2 h |
| s | supplementary raw block from late data | 2 h |
| p | partial rollup | 2 h |
| c | compacted rollup | day (1m), ISO week starting Monday (10m), month (1h) |

For one window, the main block with the highest generation wins, and every supplementary block merges into it (the main block wins on equal timestamps). For rollups, a partial overrides a compacted block inside its own 2 h window, and the highest partial generation wins.

## Block layout

```
header   32 bytes
groups   one section per collector group
series   one section per series
footer
trailer  12 bytes
```

Header:

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `ADTB` |
| 4 | 2 | version, 1 |
| 6 | 1 | tier: 0 raw, 1 = 1m, 2 = 10m, 3 = 1h |
| 7 | 1 | kind: 1 main, 2 supplementary, 3 partial, 4 compacted |
| 8 | 4 | node ID |
| 12 | 8 | window start, unix ns |
| 20 | 8 | window end, unix ns |
| 28 | 4 | generation |

Group section: `u8 group, uvarint count, uvarint length, timestamps, u32 CRC32 of the section before it`. Groups: 1 metrics, 2 REST, 3 round, 4 process, 5 process network, 6 host, 7 log, 8 peers, 9 self, 10 block headers. Each group samples on its own clock and has its own timestamp column. In rollup blocks the column holds bucket start times.

Timestamps: the first is a varint of unix nanoseconds. Each later one is a varint of the delta of deltas.

Series section:

```
u32 series ID
u8  group
u8  kind: 1 gauge, 2 counter, 3 histogram part
u8  flags: bit 0 set means counter start state follows
u8  number of value columns
[f64 offset, f64 last raw value, i64 last time]   counter start state
uvarint length, presence bitmap
per column: uvarint length, Gorilla XOR values
u32 CRC32 of the section before it
```

The presence bitmap is `uvarint runs` followed by that many uvarint run lengths. Runs alternate present, absent, present, and the first run may be zero. The run lengths sum to the group's timestamp count. A series absent from a scrape is a gap, never a zero.

Values are Gorilla XOR floats in a big-endian bit stream: the first value in 64 bits; then a 0 bit for an unchanged value; or `1 0` and the meaningful bits inside the previous leading and trailing zero window; or `1 1`, 5 bits of leading zeros, 6 bits of meaningful bit count (64 is stored as 0) and the meaningful bits. The decoder reads exactly as many values as the presence bitmap has present bits.

Columns by tier and kind:

| Tier | Gauge | Counter | Histogram part |
|---|---|---|---|
| raw | value | value | value |
| rollup | min, max, sum, count, last | last, max_rate | last |

Rollup `last` for counters and histogram parts is a reset-adjusted cumulative value, so a rate from two buckets across a restart is correct. `max_rate` is the highest per-sample rate inside the bucket. The counter start state in raw blocks is the adjustment state at the window start: the offset added so far, the last raw value and its time.

Footer:

```
uvarint number of groups,  per group: u8 group, uvarint offset, uvarint count
uvarint number of series,  per series: u32 ID, uvarint offset
i64 min time, i64 max time
uvarint number of resets,  varint restart times (unix ns)
```

A section ends where the next section begins, or at the footer. Trailer: `u32 footer length, u32 CRC32 of every byte before the trailer, "ADTE"`.

A restart time marks every counter of the node reset at that moment. The rate across a reset is the new value divided by the interval.

## series/index.bin

A sequence of records: `u32 body length, u32 CRC32 of the body, body`. Body: `u32 series ID, u32 node ID, u8 kind, u8 group, uvarint name length, name, uvarint labels length, labels`. IDs start at 1 and increase by one. Labels use the canonical form `a=b,c=d`, sorted by name, with `\`, `,` and `=` in values escaped by a backslash. A torn final record is truncated on open.

## WAL segments

The same record framing as the index. Body: `u8 type, u64 sequence, u32 node ID`, then:

- type 1, batch: `u8 group, u8 flags (1 restart, 2 late), i64 time, uvarint count, count x (uvarint series ID, u64 float bits)`.
- type 2, per-peer usage: `uvarint count`, then per sample `varint time, string key, string network, string direction, string address, varint bytes in, varint bytes out, uvarint stream count, strings`. A string is a uvarint length and bytes.

`state/watermarks.json` holds, per node, the newest sequence flushed to blocks for head records, late records and usage records. Startup replays records above those watermarks. A segment is deleted once every node with records in it has flushed past them.

## Event, session and log files

Records: `u32 body length, u32 CRC32 of the body, JSON body`. Events are `model.Event` (time, node, kind, severity, msg, attrs). Sessions are `model.Session`. Kept log lines are `{time, node, level, template, line}`. A record with a bad CRC is skipped.

## Per-peer usage files

`ADTU, u32 node ID, i64 window start, uvarint key count`, then per key: strings key, network, direction and address; `uvarint sample count`; and per sample `varint time delta, varint bytes in delta, varint bytes out delta, u8 streams changed, [uvarint count, strings]`. Deltas start from the window start and zero. A CRC32 of everything before it ends the file.

## Counter state

`state/ctr-n<node>.bin`: `uvarint count`, then per series `u32 ID, f64 offset, f64 last raw value, i64 last time`. It is the adjustment state at the end of the last flushed window.
