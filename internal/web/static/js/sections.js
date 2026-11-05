// Chart definitions for the Node page, grouped into its collapsible
// sections. Each panel names the stored metrics it needs; a panel whose
// metrics are absent on this node is hidden, so branch-only metrics appear
// by themselves when present.

import { get, qs } from "./api.js";
import { FLOW, IN, OUT, PAL, OTHER, dirColor, slot } from "./colors.js";
import { fmt } from "./format.js";
import { isVal } from "./chart/frame.js";

const dirName = (l) => (l.dir === "in" || l.dir === "inbound" ? "inbound" : "outbound");
const isOut = (l) => !(l.dir === "in" || l.dir === "inbound");

const BAR_STEPS = [60e3, 5 * 60e3, 10 * 60e3, 30 * 60e3, 3600e3, 6 * 3600e3, 86400e3, 7 * 86400e3];

// disconnectBars counts disconnect events per bucket, stacked by reason.
function disconnectBars(net) {
  return async (view, pts, panel) => {
    const span = view.to - view.from;
    const want = span / Math.max(6, Math.min(150, Math.floor(pts / 8)));
    const step = BAR_STEPS.find((s) => s >= want) || BAR_STEPS[BAR_STEPS.length - 1];
    const from = Math.floor(view.from / step) * step;
    const n = Math.ceil((view.to - from) / step);
    const ep = { node: view.node, kind: "disconnect", from: Math.floor(from), to: Math.ceil(view.to), limit: 20000 };
    const r = await get("/api/events", ep);
    panel.chart.setExports([{ label: "JSON", href: "/api/events" + qs(ep), title: "The disconnect events behind this chart" }]);
    const evs = (r.events || []).filter((e) => !net || (e.attrs && e.attrs.network === net));
    const totals = new Map();
    for (const e of evs) {
      const k = (e.attrs && e.attrs.reason) || "unknown";
      totals.set(k, (totals.get(k) || 0) + 1);
    }
    const top = [...totals.entries()].sort((a, b) => b[1] - a[1]).map((x) => x[0]);
    const keep = top.slice(0, top.length > 6 ? 5 : 6);
    const names = keep.concat(top.length > keep.length ? ["other"] : []);
    const t = Array.from({ length: n }, (_, i) => from + (i + 0.5) * step);
    const series = names.map((nm, i) => ({ name: nm, color: nm === "other" ? OTHER : slot(i), t, v: new Array(n).fill(0) }));
    const idx = new Map(names.map((nm, i) => [nm, i]));
    for (const e of evs) {
      const b = Math.floor((e.time / 1e6 - from) / step);
      if (b < 0 || b >= n) continue;
      const k = (e.attrs && e.attrs.reason) || "unknown";
      const si = idx.has(k) ? idx.get(k) : idx.get("other");
      series[si].v[b]++;
    }
    const lim = evs.length >= 20000 ? " (first 20000)" : "";
    return { from, to: from + n * step, step, series, parts: [], note: evs.length + " disconnects, " + fmtStep(step) + " buckets" + lim };
  };
}

function fmtStep(ms) {
  if (ms >= 86400e3) return ms / 86400e3 + " d";
  if (ms >= 3600e3) return ms / 3600e3 + " h";
  return ms / 60e3 + " min";
}

// peerTraffic stacks the top peers plus "other" from per-peer usage.
function peerTraffic(dir) {
  return async (view, pts, panel) => {
    const points = Math.max(20, Math.min(300, Math.floor(pts / 3)));
    const ep = { node: view.node, from: Math.floor(view.from), to: Math.ceil(view.to), points, top: 5, dir };
    const r = await get("/api/peers/usage", ep);
    panel.chart.setExports([{ label: "JSON", href: "/api/peers/usage" + qs(ep), title: "Per-peer rates behind this chart" }]);
    const step = (view.to - view.from) / points;
    const t = (r.t || []).map((x) => x / 1e6 + step / 2);
    const series = (r.series || []).map((sr, i) => ({ name: sr.key === "other" ? "other" : sr.addr || sr.key, color: sr.key === "other" ? OTHER : slot(i), t, v: sr.rate || [] }));
    return { from: view.from, to: view.to, step, series, parts: [], top: r.top || [] };
  };
}

// histHeat draws a stored histogram as a heatmap of bin counts over time.
function histHeat(metric, unit, scale) {
  return async (view, pts, panel) => {
    const cols = Math.max(20, Math.min(240, Math.floor(pts / 5)));
    const ep = { node: view.node, metric, fn: "heatmap", from: Math.floor(view.from), to: Math.ceil(view.to), points: cols };
    const r = await get("/api/series", ep);
    // CSV has no shape for bins, so heatmaps export JSON only.
    panel.chart.setExports([{ label: "JSON", href: "/api/export" + qs(Object.assign({ format: "json" }, ep)) }]);
    const hm = r.heatmaps || [];
    const step = r.step / 1e6;
    if (!hm.length) return { from: view.from, to: view.to, step, t: [], rows: [], parts: [] };
    const le = hm[0].le;
    const rows = le.map((v, i) => ({
      label: (i === le.length - 1 && v === null ? "> " + fmt(le[i - 1] * (scale || 1), unit) : "<= " + fmt(v * (scale || 1), unit)),
      short: v === null ? "inf" : fmt(v * (scale || 1), unit),
      values: new Array(cols).fill(0),
    }));
    for (const h of hm) {
      h.counts.forEach((c, b) => c.forEach((v, j) => {
        if (isVal(v) && rows[j]) rows[j].values[b] += v;
      }));
    }
    const t = hm[0].t.map((x) => x / 1e6 + step / 2);
    return { from: r.from / 1e6, to: r.to / 1e6, step, t, rows, parts: (r.parts || []).map((p) => ({ from: p.from / 1e6, to: p.to / 1e6, tier: p.tier })) };
  };
}

// logHeat draws log template counts over time, one row per template.
function logHeat(page) {
  return async (view, pts, panel) => {
    // Log counts are written once a minute, so buckets are at least that wide.
    const cols = Math.max(2, Math.min(240, Math.floor(pts / 5), Math.floor((view.to - view.from) / 60e3)));
    const ep = { node: view.node, metric: "log_template_lines_total", fn: "increase", from: Math.floor(view.from), to: Math.ceil(view.to), points: cols, limit: 2000 };
    const r = await get("/api/series", ep);
    panel.chart.setExports([{ label: "CSV", href: "/api/export" + qs(Object.assign({ format: "csv" }, ep)) }, { label: "JSON", href: "/api/export" + qs(Object.assign({ format: "json" }, ep)) }]);
    const step = r.step / 1e6;
    const rank = page.templateRank || new Map();
    let rows = (r.series || []).map((sr) => {
      const values = sr.avg.map((v) => (isVal(v) ? v : 0));
      const total = values.reduce((a, b) => a + b, 0);
      const key = (sr.labels.level || "") + "\u0000" + (sr.labels.template || "");
      return { key, total, values, label: (sr.labels.level || "?") + ": " + (sr.labels.template || "") };
    }).filter((x) => x.total > 0);
    // Rank order from the template table when it is loaded, so row labels
    // and table numbers agree.
    const rk = (x) => (rank.has(x.key) ? rank.get(x.key) : Infinity);
    rows.sort((a, b) => rk(a) - rk(b) || b.total - a.total);
    const top = rows.slice(0, 30);
    if (rows.length > 30) {
      const other = { label: rows.length - 30 + " other templates", short: "rest", values: new Array(cols).fill(0) };
      for (const x of rows.slice(30)) x.values.forEach((v, i) => (other.values[i] += v));
      top.push(other);
    }
    for (const x of top) if (!x.short) x.short = rank.has(x.key) ? "#" + rank.get(x.key) : "?";
    const t = ((r.series && r.series[0] && r.series[0].t) || []).map((x) => x / 1e6 + step / 2);
    // The most frequent template draws at the top.
    return { from: r.from / 1e6, to: r.to / 1e6, step, t, rows: top.reverse(), parts: (r.parts || []).map((p) => ({ from: p.from / 1e6, to: p.to / 1e6, tier: p.tier })) };
  };
}

const tpbSmall = (id) => ({
  id, title: "Transactions per block", unit: "txns", type: "line", height: 110, wide: true, context: true,
  sub: "How busy the network is, on the same time axis as the charts below",
  q: [{ metric: "algodtop_block_txns", name: "txns per block", color: PAL[1] }],
});

const levelColor = (l) => (l.level === "error" || l.level === "fatal" || l.level === "panic" ? PAL[5] : l.level === "warning" ? PAL[3] : l.level === "info" ? PAL[0] : PAL[4]);

export function sections(page, node) {
  return [
    {
      id: "consensus", title: "Consensus",
      panels: [
        { id: "round-time", wide: true, title: "Round time", unit: "s", type: "line", band: true, zero: false, height: 190,
          sub: "Gap between local arrival of consecutive rounds; the band is the min and max per bucket",
          q: [{ metric: "algodtop_round_time_seconds", name: "round time", color: PAL[0] }] },
        { id: "lag", title: "Sync lag", unit: "s", type: "line", band: true, sub: "Time since the last round",
          q: [{ metric: "algodtop_sync_lag_seconds", name: "lag", color: PAL[4] }] },
        { id: "votes", title: "Votes and proposals handled", unit: "/s", type: "line",
          q: [{ metric: "algod_agreement_handled_vote", fn: "rate", name: "votes", color: PAL[0], exportLabel: "votes" },
            { metric: "algod_agreement_handled_proposal", fn: "rate", name: "proposals", color: PAL[1], exportLabel: "proposals" }] },
        { id: "tpb", title: "Transactions per block", unit: "txns", type: "line", band: true,
          q: [{ metric: "algodtop_block_txns", name: "txns per block", color: PAL[1] }] },
        { id: "pool", title: "Transaction pool size", unit: "txns", type: "line", band: true,
          q: [{ metric: "algodtop_txpool_size", name: "pool", color: PAL[2] }] },
      ],
    },
    {
      id: "ws", title: "Network WS",
      panels: [
        { id: "ws-peers", title: "WebSocket peers", unit: "peers", type: "step",
          q: [{ metric: "algodtop_peers", match: ["net:ws"], name: dirName, color: (l) => dirColor(l.dir, "ws") }] },
        { id: "ws-bytes", title: "WebSocket bytes", unit: "B/s", type: "area", sub: "Received above the axis, sent below",
          q: [{ metric: "algod_network_received_bytes_total", fn: "rate", name: "received", color: FLOW["in-ws"], exportLabel: "in" },
            { metric: "algod_network_sent_bytes_total", fn: "rate", name: "sent", color: FLOW["out-ws"], neg: true, exportLabel: "out" }] },
        { id: "ws-disc", title: "WebSocket disconnect reasons", unit: "disconnects", type: "bars", metrics: [], always: true, logs: true,
          sub: "From node.log disconnect lines", load: disconnectBars("ws"), emptyText: "No WebSocket disconnects in this range" },
        { id: "ws-dropped", title: "Connections dropped before a message", unit: "/s", type: "line",
          q: [{ metric: "algod_network_connections_dropped_total", fn: "rate", name: (l) => l.reason || "dropped" }] },
      ],
    },
    {
      id: "p2p", title: "Network P2P",
      panels: [
        { id: "p2p-peers", title: "P2P connections", unit: "peers", type: "step",
          q: [{ metric: "algodtop_peers", match: ["net:p2p"], name: dirName, color: (l) => dirColor(l.dir, "p2p") }] },
        { id: "p2p-bytes", title: "P2P bytes", unit: "B/s", type: "area", sub: "Received above the axis, sent below",
          q: [{ metric: "algod_network_p2p_received_bytes_total", fn: "rate", name: "received", color: FLOW["in-p2p"], exportLabel: "in" },
            { metric: "algod_network_p2p_sent_bytes_total", fn: "rate", name: "sent", color: FLOW["out-p2p"], neg: true, exportLabel: "out" }] },
        { id: "gossip", title: "Gossip streams", unit: "streams", type: "step", sub: "libp2p streams on /meshsub",
          q: [{ metric: "libp2p_rcmgr_streams", match: ["protocol:/meshsub*"], by: "dir", name: dirName, color: (l) => dirColor(l.dir, "p2p") }] },
        { id: "idle", title: "Idle peers", unit: "peers", type: "step", sub: "Connected with no open gossip stream",
          q: [{ metric: "algodtop_peers_idle", name: "idle", color: PAL[3] }] },
        { id: "dht", title: "DHT network size", unit: "peers", type: "step",
          q: [{ metric: "libp2p_io_dht_kad_network_size", name: "DHT size", color: PAL[4] }] },
        { id: "evict", title: "Inbound evictions", unit: "/s", type: "line", sub: "Inbound connections closed to make room, by state",
          q: [{ metric: "algod_network_p2p_inbound_evictions_total", fn: "rate", name: (l) => l.state || "evictions" }] },
        { id: "setup-to", title: "Stream setup timeouts", unit: "/s", type: "line", sub: "Dialed connections closed for carrying no gossip stream",
          q: [{ metric: "algod_network_p2p_stream_setup_timeouts_total", fn: "rate", name: "timeouts", color: PAL[5] }] },
        { id: "rcmgr", title: "Resource manager blocks", unit: "blocks", type: "line",
          q: [{ metric: "libp2p_rcmgr_blocked_resources", name: (l) => [l.resource, l.dir, l.scope].filter(Boolean).join(" "), color: (l, i) => slot(i) }] },
        { id: "p2p-disc", title: "P2P disconnect reasons", unit: "disconnects", type: "bars", metrics: [], always: true, logs: true,
          sub: "From node.log disconnect lines", load: disconnectBars("p2p"), emptyText: "No P2P disconnects in this range" },
        { id: "dht-lat", title: "DHT RPC latency", type: "heatmap", unitLabel: "outbound requests per bin", metrics: ["libp2p_io_dht_kad_rpc_outbound_request_latency_milliseconds_bucket"],
          load: histHeat("libp2p_io_dht_kad_rpc_outbound_request_latency_milliseconds_bucket", "s", 1e-3), cellUnit: "requests" },
        { id: "dial-lat", title: "Dial latency", type: "heatmap", unitLabel: "dials per bin", metrics: ["libp2p_swarm_dial_latency_seconds_bucket"],
          load: histHeat("libp2p_swarm_dial_latency_seconds_bucket", "s", 1), cellUnit: "dials" },
      ],
    },
    {
      id: "bandwidth", title: "Bandwidth",
      panels: [
        tpbSmall("bw-tpb"),
        { id: "bw-app", wide: true, title: "App-level bytes by network", unit: "B/s", type: "area", height: 200,
          sub: "algod counters, WebSocket and P2P stacked; received above the axis, sent below",
          q: [{ metric: "algod_network_received_bytes_total", fn: "rate", name: "WS received", color: FLOW["in-ws"], exportLabel: "ws in" },
            { metric: "algod_network_p2p_received_bytes_total", fn: "rate", name: "P2P received", color: FLOW["in-p2p"], exportLabel: "p2p in" },
            { metric: "algod_network_sent_bytes_total", fn: "rate", name: "WS sent", color: FLOW["out-ws"], neg: true, exportLabel: "ws out" },
            { metric: "algod_network_p2p_sent_bytes_total", fn: "rate", name: "P2P sent", color: FLOW["out-p2p"], neg: true, exportLabel: "p2p out" }] },
        { id: "bw-wire", title: "Process wire bytes", unit: "B/s", type: "area", minStep: 60e3,
          note: (series) => {
            const m = series.find((x) => x.labels && x.labels.method);
            return m ? "method: " + m.labels.method : "";
          },
          q: [{ metric: "proc_net_bytes_total", fn: "rate", name: (l) => (isOut(l) ? "sent" : "received"), color: (l) => (isOut(l) ? OUT : IN), neg: isOut }] },
        { id: "bw-gap", title: "Wire minus app-level", unit: "B/s", type: "line", metrics: ["proc_net_bytes_total"], minStep: 60e3,
          sub: "libp2p overhead: DHT, gossipsub control and encryption",
          q: [{ metric: "proc_net_bytes_total", fn: "rate", id: "wire" },
            { metric: "algod_network_received_bytes_total", fn: "rate", id: "wsi" },
            { metric: "algod_network_p2p_received_bytes_total", fn: "rate", id: "p2i" },
            { metric: "algod_network_sent_bytes_total", fn: "rate", id: "wso" },
            { metric: "algod_network_p2p_sent_bytes_total", fn: "rate", id: "p2o" }],
          derive: (series) => {
            const wire = series.filter((x) => x.key.startsWith("wire|"));
            const pick = (id) => series.find((x) => x.key.startsWith(id + "|"));
            const out = [];
            for (const [dir, ids, color, name] of [["in", ["wsi", "p2i"], IN, "received overhead"], ["out", ["wso", "p2o"], OUT, "sent overhead"]]) {
              const w = wire.find((x) => (dir === "in" ? !isOut(x.labels) : isOut(x.labels)));
              if (!w) continue;
              const apps = ids.map(pick).filter(Boolean);
              const v = w.v.map((x, i) => {
                if (!isVal(x)) return null;
                let a = 0;
                for (const s of apps) if (isVal(s.v[i])) a += s.v[i];
                return x - a;
              });
              out.push({ key: "gap|" + dir, name, color, t: w.t, v, labels: {} });
            }
            return out;
          } },
        { id: "bw-peer-in", title: "Received by peer", unit: "B/s", type: "area", metrics: [], usage: true,
          sub: "Top 5 peers stacked plus other, from per-peer usage", load: peerTraffic("in") },
        { id: "bw-peer-out", title: "Sent by peer", unit: "B/s", type: "area", metrics: [], usage: true,
          sub: "Top 5 peers stacked plus other, from per-peer usage", load: peerTraffic("out") },
        { id: "bw-host", title: "Host interface bytes", unit: "B/s", type: "area", minStep: 60e3, sub: "Every process on the host; received above the axis, sent below",
          q: [{ metric: "host_net_bytes_total", fn: "rate", name: (l) => (isOut(l) ? "sent" : "received"), color: (l) => (isOut(l) ? OUT : IN), neg: isOut }] },
      ],
    },
    {
      id: "resources", title: "Resources",
      panels: [
        tpbSmall("res-tpb"),
        { id: "cpu", title: "CPU", unit: "%", type: "line", sub: "Percent of one core",
          q: [{ metric: "proc_cpu_seconds_total", fn: "rate", scale: 100, name: "algod CPU", color: PAL[0] }] },
        { id: "rss", title: "Resident memory", unit: "bytes", type: "line", band: true,
          q: [{ metric: "proc_resident_bytes", name: "RSS", color: PAL[4] }] },
        { id: "fds", title: "Open handles", unit: "handles", type: "step", band: true,
          q: [{ metric: "proc_open_handles", name: "file descriptors", color: PAL[2] }] },
        { id: "threads", title: "Threads", unit: "threads", type: "step",
          q: [{ metric: "proc_threads", name: "threads", color: PAL[1] }] },
        { id: "diskio", title: "Disk I/O", unit: "B/s", type: "line",
          q: [{ metric: "proc_read_bytes_total", fn: "rate", name: "read", color: PAL[4], exportLabel: "read" },
            { metric: "proc_write_bytes_total", fn: "rate", name: "write", color: PAL[0], exportLabel: "write" }] },
        { id: "disk-free", title: "Disk free", unit: "bytes", type: "line", minStep: 60e3,
          q: [{ metric: "host_disk_free_bytes", name: "free", color: PAL[1], exportLabel: "free" },
            { metric: "host_disk_total_bytes", name: "size", color: OTHER, dash: true, exportLabel: "size" }] },
        { id: "datadir", title: "Data dir size", unit: "bytes", type: "area", sub: "By subdirectory, measured every 10 minutes", minStep: 600e3,
          q: [{ metric: "host_datadir_bytes", name: (l) => l.sub || "data" }] },
        { id: "load", title: "Host load average", unit: "", type: "line", unitLabel: "runnable tasks",
          q: [{ metric: "host_load1", name: "1 min", color: PAL[0] }, { metric: "host_load15", name: "15 min", color: PAL[4] }] },
      ],
    },
    {
      id: "logs", title: "Logs",
      panels: [
        { id: "log-heat", wide: true, title: "Log templates over time", type: "heatmap", unitLabel: "lines per bucket", height: 260,
          metrics: ["log_template_lines_total"], load: logHeat(page), cellUnit: "lines",
          sub: "One row per template; row labels match the # column in the table below" },
        { id: "log-levels", title: "Log lines by level", unit: "/min", type: "line", minStep: 60e3,
          q: [{ metric: "log_lines_total", fn: "rate", scale: 60, name: (l) => l.level || "lines", color: levelColor }] },
        { id: "log-flood", title: "Invalid pool transactions by app", unit: "/min", type: "line", minStep: 60e3,
          q: [{ metric: "log_txpool_invalid_total", fn: "rate", scale: 60, name: (l) => "app " + (l.app || "?") }] },
      ],
    },
  ];
}
