// Compare: two time ranges of one node aligned to their start (run 2 vs
// run 3), or two nodes over one range. Each metric gets an overlay chart
// and a summary row of mean, p50, p99 and the difference.

import { h, clear, sortableTable } from "../dom.js";
import { get, qs } from "../api.js";
import { rangeOf, setParams, encodeTime, decodeTime, parseDur, durName } from "../state.js";
import { fmt, toLocalInput, isNum } from "../format.js";
import { nodePicker, rangeBar } from "../common.js";
import { Bus } from "../chart/crosshair.js";
import { Panel } from "../panel.js";
import { PAL } from "../colors.js";

const METRICS = [
  { id: "rtime", label: "Round time", metric: "algodtop_round_time_seconds", unit: "s" },
  { id: "lag", label: "Sync lag", metric: "algodtop_sync_lag_seconds", unit: "s" },
  { id: "tpb", label: "Transactions per block", metric: "algodtop_block_txns", unit: "txns" },
  { id: "pool", label: "Transaction pool size", metric: "algodtop_txpool_size", unit: "txns" },
  { id: "peers", label: "Peers", metric: "algodtop_peers", by: "dir", unit: "peers" },
  { id: "cpu", label: "CPU", metric: "proc_cpu_seconds_total", fn: "rate", scale: 100, unit: "%" },
  { id: "rss", label: "Resident memory", metric: "proc_resident_bytes", unit: "bytes" },
  { id: "wsin", label: "WebSocket received", metric: "algod_network_received_bytes_total", fn: "rate", unit: "B/s" },
  { id: "wsout", label: "WebSocket sent", metric: "algod_network_sent_bytes_total", fn: "rate", unit: "B/s" },
  { id: "p2pin", label: "P2P received", metric: "algod_network_p2p_received_bytes_total", fn: "rate", unit: "B/s" },
  { id: "p2pout", label: "P2P sent", metric: "algod_network_p2p_sent_bytes_total", fn: "rate", unit: "B/s" },
  { id: "votes", label: "Votes handled", metric: "algod_agreement_handled_vote", fn: "rate", unit: "/s" },
];

const A_COLOR = PAL[0], B_COLOR = PAL[4];

export function mount(root, app, r) {
  let q = r.q;
  const bus = new Bus();
  const ctx = { events: [], annotations: [], regions: [] };
  const head = h("div", { class: "page-head" });
  const ctl = h("div");
  const sumEl = h("div");
  const grid = h("div", { class: "grid" });
  root.appendChild(h("div", { id: "compare" }, head, ctl, h("h2", { style: { margin: "12px 0 8px" } }, "Summary"), sumEl, h("h2", { style: { margin: "16px 0 8px" } }, "Overlays"), grid));
  let panels = [];
  let destroyed = false;

  function setup() {
    const mode = q.get("mode") === "nodes" ? "nodes" : "ranges";
    const nodeA = q.get("node") || app.lastNode || (app.nodes[0] && app.nodes[0].name) || "";
    const nodeB = q.get("node2") || (app.nodes.find((n) => n.name !== nodeA) || {}).name || nodeA;
    const now = Date.now();
    let A, B, d;
    if (mode === "ranges") {
      d = parseDur(q.get("d") || "") || 6 * 3600e3;
      B = { node: nodeA, from: decodeTime(q.get("b")) || now - d };
      A = { node: nodeA, from: decodeTime(q.get("a")) || B.from - 86400e3 };
      A.to = A.from + d;
      B.to = B.from + d;
    } else {
      const rg = rangeOf(q, { def: "6h" });
      A = { node: nodeA, from: rg.from, to: rg.to };
      B = { node: nodeB, from: rg.from, to: rg.to };
      d = rg.to - rg.from;
    }
    return { mode, A, B, d, nodeA, nodeB };
  }

  function label(c, which) {
    const x = which === "A" ? c.A : c.B;
    if (c.mode === "nodes") return x.node;
    return which + " " + toLocalInput(x.from).replace("T", " ");
  }

  function drawControls(c) {
    clear(head);
    head.append(h("h1", null, "Compare"), h("div", { class: "seg" },
      h("button", { type: "button", class: c.mode === "ranges" ? "on" : "", on: { click: () => setParams({ mode: null }) } }, "two ranges, one node"),
      h("button", { type: "button", class: c.mode === "nodes" ? "on" : "", on: { click: () => setParams({ mode: "nodes" }) } }, "two nodes, one range")));
    clear(ctl);
    if (c.mode === "ranges") {
      const a = h("input", { type: "datetime-local", value: toLocalInput(c.A.from) });
      const b = h("input", { type: "datetime-local", value: toLocalInput(c.B.from) });
      const d = h("input", { type: "text", value: durName(c.d), style: { width: "70px" }, "aria-label": "Length" });
      ctl.appendChild(h("form", { class: "bar", on: { submit: (e) => {
        e.preventDefault();
        setParams({ a: encodeTime(Date.parse(a.value)), b: encodeTime(Date.parse(b.value)), d: d.value.trim() || null });
      } } },
      h("span", { class: "muted" }, "node"), nodePicker(app.nodes, c.nodeA, (v) => setParams({ node: v })),
      h("span", { class: "key", style: { background: A_COLOR, width: "12px", height: "3px", display: "inline-block" } }), "A starts", a,
      h("span", { class: "key", style: { background: B_COLOR, width: "12px", height: "3px", display: "inline-block" } }), "B starts", b,
      "length", d, h("button", { type: "submit", class: "primary" }, "compare"),
      h("span", { class: "note" }, "Both ranges are aligned to their start.")));
    } else {
      ctl.append(h("div", { class: "bar" },
        h("span", { class: "key", style: { background: A_COLOR, width: "12px", height: "3px", display: "inline-block" } }),
        nodePicker(app.nodes, c.nodeA, (v) => setParams({ node: v }), { label: "First node" }),
        h("span", { class: "muted" }, "vs"),
        h("span", { class: "key", style: { background: B_COLOR, width: "12px", height: "3px", display: "inline-block" } }),
        nodePicker(app.nodes, c.nodeB, (v) => setParams({ node2: v }), { label: "Second node" })),
      rangeBar(q, { def: "6h", restart: false }));
    }
  }

  async function drawSummary(c) {
    clear(sumEl);
    sumEl.appendChild(h("p", { class: "note" }, "Loading..."));
    const raw = q.get("raw") === "1" ? "raw" : "";
    const one = (m, x) => get("/api/summary", { node: x.node, metric: m.metric, fn: m.fn, by: m.by, from: Math.floor(x.from), to: Math.ceil(x.to), tier: raw }).catch(() => null);
    const res = await Promise.all(METRICS.map((m) => Promise.all([one(m, c.A), one(m, c.B)])));
    if (destroyed) return;
    const rows = [];
    METRICS.forEach((m, i) => {
      const [ra, rb] = res[i];
      const sa = (ra && ra.summary) || {}, sb = (rb && rb.summary) || {};
      const keys = [...new Set(Object.keys(sa).concat(Object.keys(sb)))].sort();
      for (const k of keys) {
        const a = sa[k] || {}, b = sb[k] || {};
        const k2 = m.scale || 1;
        const sa1 = (x, v) => (x.n && isNum(v) ? v * k2 : null);
        if (!a.n && !b.n) continue;
        const part = m.by ? (k.split(",").find((p) => p.startsWith(m.by + "=")) || "").slice(m.by.length + 1) : "";
        rows.push({ name: m.label + (part ? " (" + part + ")" : ""), unit: m.unit,
          am: sa1(a, a.mean), a50: sa1(a, a.p50), a99: sa1(a, a.p99), bm: sa1(b, b.mean), b50: sa1(b, b.p50), b99: sa1(b, b.p99), an: a.n || 0, bn: b.n || 0 });
      }
    });
    const f = (v, u) => (v === null ? "-" : fmt(v, u));
    const diff = (x) => {
      if (x.am === null || x.bm === null) return null;
      return x.bm - x.am;
    };
    const pct = (x) => {
      const d = diff(x);
      if (d === null || !x.am) return null;
      return (d / Math.abs(x.am)) * 100;
    };
    clear(sumEl);
    sumEl.appendChild(h("div", { class: "table-wrap" }, sortableTable([
      { key: "name", label: "metric" },
      { key: "am", label: "A mean", num: true, render: (x) => f(x.am, x.unit), title: label(c, "A") },
      { key: "a50", label: "A p50", num: true, render: (x) => f(x.a50, x.unit) },
      { key: "a99", label: "A p99", num: true, render: (x) => f(x.a99, x.unit) },
      { key: "bm", label: "B mean", num: true, render: (x) => f(x.bm, x.unit), title: label(c, "B") },
      { key: "b50", label: "B p50", num: true, render: (x) => f(x.b50, x.unit) },
      { key: "b99", label: "B p99", num: true, render: (x) => f(x.b99, x.unit) },
      { key: "diff", label: "B - A (mean)", num: true, get: pct, render: (x) => {
        const d = diff(x), p = pct(x);
        if (d === null) return "-";
        return (d >= 0 ? "+" : "-") + fmt(Math.abs(d), x.unit) + (p === null ? "" : " (" + (p >= 0 ? "+" : "") + p.toFixed(1) + "%)");
      } },
      { key: "an", label: "buckets A / B", num: true, render: (x) => x.an + " / " + x.bn },
    ], rows, { empty: "No data in either range." })));
    sumEl.appendChild(h("p", { class: "note" }, "A is " + label(c, "A") + ", B is " + label(c, "B") + ". Percentiles are over the chart buckets."));
  }

  function buildCharts(c) {
    for (const p of panels) p.destroy();
    panels = [];
    clear(grid);
    const page = { bus, ctx, relative: c.mode === "ranges" };
    const raw = q.get("raw") === "1";
    for (const m of METRICS) {
      const spec = {
        id: m.id, title: m.label, unit: m.unit, type: "line", noExport: true,
        sub: c.mode === "ranges" ? "Time from the start of each range" : "",
        load: async (view, pts) => {
          const one = async (x, color, tag) => {
            const params = { node: x.node, metric: m.metric, fn: m.fn, by: m.by, from: Math.floor(x.from), to: Math.ceil(x.to), points: pts, tier: raw ? "raw" : "" };
            const res = await get("/api/series", params);
            const shift = c.mode === "ranges" ? x.from : 0;
            const step = res.raw ? 0 : res.step / 1e6;
            return { params, series: (res.series || []).map((sr) => ({
              key: tag + JSON.stringify(sr.labels),
              name: label(c, tag) + (m.by && sr.labels[m.by] ? " " + sr.labels[m.by] : ""),
              color, dash: tag === "A",
              t: sr.t.map((t) => t / 1e6 - shift + step / 2),
              v: sr.avg.map((v) => (v === null ? null : v * (m.scale || 1))),
            })), step, raw: res.raw };
          };
          const [a, b] = await Promise.all([one(c.A, A_COLOR, "A"), one(c.B, B_COLOR, "B")]);
          const exp = [];
          for (const [tag, x] of [["A", a], ["B", b]]) {
            exp.push({ label: "CSV " + tag, href: "/api/export" + qs(Object.assign({}, x.params, { format: "csv" })) });
            exp.push({ label: "JSON " + tag, href: "/api/export" + qs(Object.assign({}, x.params, { format: "json" })) });
          }
          panel.chart.setExports(exp);
          const from = c.mode === "ranges" ? 0 : c.A.from, to = c.mode === "ranges" ? c.d : c.A.to;
          return { from, to, step: a.step || b.step, raw: a.raw, parts: [], series: a.series.concat(b.series), tol: (to - from) / 100 };
        },
      };
      const panel = new Panel(spec, page);
      panels.push(panel);
      grid.appendChild(panel.el);
    }
    for (const p of panels) p.load({ key: "c" });
  }

  function render() {
    const c = setup();
    drawControls(c);
    drawSummary(c);
    buildCharts(c);
    bus.zoomSubs.clear();
    bus.onZoom((a, b) => {
      if (c.mode === "ranges") setParams({ a: encodeTime(c.A.from + a), b: encodeTime(c.B.from + a), d: durName(Math.max(60e3, Math.round((b - a) / 60e3) * 60e3)) });
      else setParams({ from: encodeTime(a), to: encodeTime(b), r: null });
    });
  }

  render();
  return {
    update(r2) {
      q = r2.q;
      render();
    },
    destroy() {
      destroyed = true;
      for (const p of panels) p.destroy();
    },
  };
}
