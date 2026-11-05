// Fleet: one card per node, or a sortable table with the same data. It
// answers "is anything wrong" without scrolling: health and its reason
// come first.

import { h, clear, sortableTable } from "../dom.js";
import { get } from "../api.js";
import { setParams, nodeURL } from "../state.js";
import { fmt, fmtInt, fmtSeconds, fmtShortTime, ns, isNum } from "../format.js";
import { healthBadge, healthLevel, healthReasons, hasProc, participation } from "../common.js";
import { spark } from "../chart/spark.js";
import { IN, OUT, PAL } from "../colors.js";

const DAY = 86400e3;
const SPARK_POINTS = 96;

function sumArrays(list) {
  if (!list.length) return [];
  const n = Math.max(...list.map((a) => a.length));
  const out = new Array(n).fill(null);
  for (const a of list) {
    for (let i = 0; i < a.length; i++) {
      if (a[i] === null || a[i] === undefined) continue;
      out[i] = (out[i] || 0) + a[i];
    }
  }
  return out;
}

async function loadSparks(n) {
  const to = Date.now();
  const from = Math.max(to - DAY, n.history_from ? n.history_from / 1e6 : 0);
  const p = (metric) => get("/api/series", { node: n.name, metric, from, to, points: SPARK_POINTS }).catch(() => null);
  const [rt, wsi, p2i, wso, p2o] = await Promise.all([
    p("algodtop_round_time_seconds"),
    p("algod_network_received_bytes_total"),
    p("algod_network_p2p_received_bytes_total"),
    p("algod_network_sent_bytes_total"),
    p("algod_network_p2p_sent_bytes_total"),
  ]);
  const avg = (r) => (r && r.series && r.series[0] ? r.series[0].avg : []);
  const ts = (r) => (r && r.series && r.series[0] ? r.series[0].t.map((x) => x / 1e6) : []);
  const tt = ts(wsi).length ? ts(wsi) : ts(p2i).length ? ts(p2i) : ts(rt);
  const bw = [{ t: tt, v: sumArrays([avg(wsi), avg(p2i)]), color: IN }, { t: tt, v: sumArrays([avg(wso), avg(p2o)]), color: OUT }];
  const rts = [{ t: ts(rt), v: avg(rt), color: PAL[0] }];
  // A node with less than a day of history draws from its first sample
  // rather than as a sliver at the right edge.
  let start = to;
  for (const x of bw.concat(rts)) {
    const i = x.v.findIndex((v) => v !== null && v !== undefined);
    if (i >= 0 && x.t[i] < start) start = x.t[i];
  }
  if (start >= to) start = from;
  return { from: Math.max(from, start), to, bw, rt: rts };
}

// sparkSpan names the time a sparkline covers: "24 h", "3 h" or "40 min".
function sparkSpan(sp) {
  const ms = sp ? sp.to - sp.from : DAY;
  if (ms >= DAY - 1800e3) return "24 h";
  if (ms >= 3600e3) return Math.round(ms / 3600e3) + " h";
  return Math.max(1, Math.round(ms / 60e3)) + " min";
}

function maxVal(a) {
  let m = null;
  for (const v of a || []) if (v !== null && v !== undefined && (m === null || v > m)) m = v;
  return m;
}

function appIn(n) {
  return (n.ws_in || 0) + (n.p2p_in || 0);
}
function appOut(n) {
  return (n.ws_out || 0) + (n.p2p_out || 0);
}

export function mount(root, app, r) {
  const sparks = new Map();
  let q = r.q;
  const body = h("div");
  const head = h("div", { class: "page-head" });
  const page = h("div", { id: "fleet" }, head, body);
  root.appendChild(page);
  let timer = 0;

  function sparkEl(name, kind, label) {
    const sp = sparks.get(name);
    if (!sp) return h("div", { class: "spark muted small" }, "loading");
    const data = kind === "bw" ? sp.bw : sp.rt;
    return spark(data, sp.from, sp.to, { label, width: 160, height: 32 });
  }

  function card(n) {
    const lvl = healthLevel(n);
    const reasons = healthReasons(n);
    const sp = sparks.get(n.name);
    const kv = (k, v) => [h("span", { class: "k" }, k), h("span", { class: "v" }, v)];
    const peers = n.peers || {};
    const ev = n.last_event;
    return h("div", { class: "card " + lvl },
      h("div", { class: "card-head" }, h("a", { href: nodeURL(n.name) }, n.name), healthBadge(n)),
      reasons,
      h("div", { class: "kv" },
        kv("network", n.network || "-"), kv("version", n.version || "-"),
        kv("round", n.round ? fmtInt(n.round) : "-"), kv("lag", n.up && isNum(n.lag) ? fmtSeconds(n.lag) : "-"),
        kv("WS in/out", (peers.ws_in || 0) + " / " + (peers.ws_out || 0)), kv("P2P in/out", (peers.p2p_in || 0) + " / " + (peers.p2p_out || 0)),
        kv("round time", n.round_time ? fmtSeconds(n.round_time) : "-"), kv("CPU", !hasProc(n) ? "n/a" : n.up && isNum(n.cpu) ? n.cpu.toFixed(1) + "%" : "-")),
      h("div", { class: "sparks" },
        h("div", { class: "spark-box" }, h("div", { class: "label" }, "bandwidth " + sparkSpan(sp)),
          h("div", { class: "spark-val" }, h("span", { class: "dir-in" }, fmt(appIn(n), "B/s")), " / ", h("span", { class: "dir-out" }, fmt(appOut(n), "B/s"))),
          sparkEl(n.name, "bw", "Received (cyan) and sent (orange) app-level bytes per second, last " + sparkSpan(sp))),
        h("div", { class: "spark-box" }, h("div", { class: "label" }, "round time " + sparkSpan(sp)),
          h("div", { class: "spark-val" }, sp && isNum(maxVal(sp.rt[0].v)) ? "max " + fmt(maxVal(sp.rt[0].v), "s") : ""),
          sparkEl(n.name, "rt", "Round time, last " + sparkSpan(sp)))),
      h("div", { class: "small" }, h("span", { class: "muted" }, "participation: "), participation(n)),
      ev ? h("div", { class: "lastev", title: ev.msg }, h("span", { class: "muted" }, fmtShortTime(ns(ev.time)) + " "), h("span", { class: "sev " + (ev.severity || "info") }, ev.kind), " ", ev.msg) : h("div", { class: "lastev muted" }, "no events yet"));
  }

  const cols = [
    { key: "name", label: "NODE", render: (n) => h("a", { href: nodeURL(n.name) }, n.name) },
    { key: "network", label: "NET" },
    { key: "version", label: "VER" },
    { key: "round", label: "ROUND", num: true, render: (n) => (n.round ? fmtInt(n.round) : "-") },
    { key: "lag", label: "LAG", num: true, get: (n) => (n.up ? n.lag : null), render: (n) => (n.up && isNum(n.lag) ? fmtSeconds(n.lag) : "-") },
    { key: "round_time", label: "RTIME", num: true, render: (n) => (n.round_time ? n.round_time.toFixed(2) + "s" : "-") },
    { key: "ws", label: "WS i/o", num: true, get: (n) => (n.peers ? n.peers.ws_in + n.peers.ws_out : 0), render: (n) => (n.peers ? n.peers.ws_in + "/" + n.peers.ws_out : "-") },
    { key: "p2p", label: "P2P i/o", num: true, get: (n) => (n.peers ? n.peers.p2p_in + n.peers.p2p_out : 0), render: (n) => (n.peers ? n.peers.p2p_in + "/" + n.peers.p2p_out : "-") },
    { key: "cpu", label: "CPU%", num: true, get: (n) => (hasProc(n) && n.up ? n.cpu : null), render: (n) => (!hasProc(n) ? "n/a" : isNum(n.cpu) && n.up ? n.cpu.toFixed(1) : "-") },
    { key: "rss", label: "RSS", num: true, render: (n) => (!hasProc(n) ? "n/a" : n.rss && n.up ? fmt(n.rss, "bytes") : "-") },
    { key: "net", label: "NET KB/s", num: true, title: "App-level received / sent", get: (n) => appIn(n) + appOut(n), render: (n) => (n.up ? (appIn(n) / 1e3).toFixed(0) + "/" + (appOut(n) / 1e3).toFixed(0) : "-") },
    { key: "bw", label: "bandwidth 24 h", sort: false, render: (n) => sparkEl(n.name, "bw", "Received and sent, last " + sparkSpan(sparks.get(n.name))) },
    { key: "rt", label: "round time 24 h", sort: false, render: (n) => sparkEl(n.name, "rt", "Round time, last " + sparkSpan(sparks.get(n.name))) },
    { key: "health", label: "HEALTH", get: (n) => ({ ok: 0, warn: 1, critical: 2, down: 3 })[healthLevel(n)], render: (n) => h("div", { class: "health-cell" }, healthBadge(n), healthReasons(n)) },
  ];

  function draw() {
    const view = q.get("view") === "table" ? "table" : "cards";
    clear(head);
    head.append(h("h1", null, "Fleet"),
      h("div", { class: "seg" },
        h("button", { type: "button", class: view === "cards" ? "on" : "", on: { click: () => setParams({ view: null }) } }, "cards"),
        h("button", { type: "button", class: view === "table" ? "on" : "", on: { click: () => setParams({ view: "table" }) } }, "table")));
    clear(body);
    const nodes = app.nodes.slice();
    if (!nodes.length) {
      body.appendChild(h("div", { class: "banner" }, "No nodes are configured. Add nodes to the config file, or run algodtop daemon --discover."));
      return;
    }
    // Worst first, so a problem is on the first screen.
    const rank = { down: 0, critical: 1, warn: 2, ok: 3 };
    nodes.sort((a, b) => rank[healthLevel(a)] - rank[healthLevel(b)] || a.name.localeCompare(b.name));
    if (view === "table") {
      body.appendChild(h("div", { class: "table-wrap" }, sortableTable(cols, nodes, {
        sort: q.get("sort") || null, desc: q.get("desc") !== "0",
        onSort: (k, d) => setParams({ sort: k, desc: d ? null : "0" }, { replace: true, silent: true }),
      })));
    } else {
      body.appendChild(h("div", { class: "cards" }, nodes.map(card)));
    }
  }

  async function refreshSparks() {
    await Promise.all(app.nodes.map(async (n) => {
      try {
        sparks.set(n.name, await loadSparks(n));
      } catch (e) {
        /* a node without history keeps its placeholder */
      }
    }));
    draw();
  }

  draw();
  refreshSparks();
  timer = setInterval(refreshSparks, 5 * 60e3);
  let last = 0;

  return {
    update(r2) {
      q = r2.q;
      draw();
    },
    status() {
      // Status arrives every 2 s; redraw at most every 4 s.
      const now = Date.now();
      if (now - last < 4000) return;
      last = now;
      draw();
    },
    destroy() {
      clearInterval(timer);
    },
  };
}
