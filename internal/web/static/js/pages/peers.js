// Peers: the current peers table, the history per remote address, and
// clusters of inbound peers that disconnected together, each with its
// timeline.

import { h, s, clear, sortableTable, inlineBar, showTip, hideTip } from "../dom.js";
import { get } from "../api.js";
import { rangeOf, setParams, navigate } from "../state.js";
import { fmt, fmtDur, fmtShortTime, fmtDateTime, fmtClock, ns } from "../format.js";
import { rangeBar, nodePicker } from "../common.js";
import { linear, timeTicks } from "../chart/scale.js";
import { slot } from "../colors.js";

function dirCell(d) {
  return h("span", { class: d === "in" ? "dir-in" : "dir-out" }, d === "in" ? "in" : "out");
}

function reasonsCell(m) {
  const ents = Object.entries(m || {}).sort((a, b) => b[1] - a[1]);
  return ents.length ? ents.map(([k, v]) => h("span", { class: "chip" }, k + " " + v)) : "";
}

export function mount(root, app, r) {
  const name = r.node;
  let q = r.q;
  const head = h("div", { class: "page-head" });
  const cur = h("div");
  const rangeEl = h("div");
  const hist = h("div");
  const clus = h("div");
  root.appendChild(h("div", { id: "peers" }, head, h("h2", null, "Current peers"), cur,
    h("h2", { style: { marginTop: "20px" } }, "History"), rangeEl, hist,
    h("h2", { style: { marginTop: "20px" } }, "Clusters"), clus));
  let timer = 0;
  let destroyed = false;

  function drawHead() {
    clear(head);
    head.append(h("h1", null, "Peers"), nodePicker(app.nodes, name, (v) => navigate("/peers/" + encodeURIComponent(v) + location.search)),
      h("a", { href: "/node/" + encodeURIComponent(name) }, "node charts"));
  }

  async function drawCurrent() {
    let res;
    try {
      res = await get("/api/peers", { node: name });
    } catch (e) {
      clear(cur);
      cur.appendChild(h("p", { class: "error" }, e.message));
      return;
    }
    if (destroyed) return;
    const peers = res.peers || [];
    const usage = !!res.usage;
    const c = res.counts || {};
    const filter = (q.get("f") || "").toLowerCase();
    const list = filter ? peers.filter((p) => [p.addr, p.ip, p.peer_id, p.network, p.direction].join(" ").toLowerCase().includes(filter)) : peers;
    const maxRate = Math.max(1, ...peers.map((p) => Math.max(p.rate_in || 0, p.rate_out || 0)));
    const cols = [
      { key: "addr", label: "address", render: (p) => h("span", { class: "mono" }, p.addr) },
      { key: "direction", label: "dir", render: (p) => dirCell(p.direction) },
      { key: "network", label: "net" },
      { key: "since", label: "connected", num: true, get: (p) => -p.since, render: (p) => (p.since ? h("span", { title: p.since_before ? "Connected before algodtop first looked, so at least this long" : null }, fmtDur((Date.now() - ns(p.since)) / 1000) + (p.since_before ? "+" : "")) : "-") },
    ];
    if (usage) {
      cols.push(
        { key: "peer_id", label: "peer ID", render: (p) => h("span", { class: "mono", title: p.peer_id || "" }, p.peer_id ? p.peer_id.slice(0, 12) + "..." : "") },
        { key: "rate_in", label: "receive rate", num: true, render: (p) => inlineBar(p.rate_in || 0, maxRate, fmt(p.rate_in || 0, "B/s"), "in") },
        { key: "rate_out", label: "send rate", num: true, render: (p) => inlineBar(p.rate_out || 0, maxRate, fmt(p.rate_out || 0, "B/s"), "out") },
        { key: "bytes_in", label: "session in", num: true, render: (p) => fmt(p.bytes_in || 0, "bytes") },
        { key: "bytes_out", label: "session out", num: true, render: (p) => fmt(p.bytes_out || 0, "bytes") },
        { key: "streams", label: "streams", num: true, get: (p) => (p.streams || []).length, render: (p) => h("span", { title: (p.streams || []).join("\n") }, String((p.streams || []).length)) },
        { key: "protocols", label: "protocols", cls: "wrap small", get: (p) => (p.protocols || []).length, render: (p) => (p.protocols || []).join(" ") });
    }
    const fi = h("input", { type: "search", placeholder: "filter", value: q.get("f") || "", on: { change: (e) => setParams({ f: e.target.value || null }, { replace: true }) } });
    clear(cur);
    cur.append(h("div", { class: "bar" },
      h("span", null, peers.length + " peers: WS " + (c.ws_in || 0) + " in / " + (c.ws_out || 0) + " out, P2P " + (c.p2p_in || 0) + " in / " + (c.p2p_out || 0) + " out"),
      usage ? null : h("span", { class: "note" }, "Per-peer usage is not available on this node, so rates and peer IDs are not shown."),
      h("span", { class: "grow" }), fi),
    h("div", { class: "table-wrap", style: { maxHeight: "480px", overflowY: "auto" } }, sortableTable(cols, list, { sort: usage ? "rate_in" : "since", desc: true, empty: "No peers." })));
  }

  async function drawHistory() {
    const rg = rangeOf(q, { def: "24h" });
    clear(rangeEl);
    rangeEl.appendChild(rangeBar(q, { def: "24h", raw: false, node: app.byName(name) }));
    let res;
    try {
      res = await get("/api/peers/history", { node: name, from: Math.floor(rg.from), to: Math.ceil(rg.to) });
    } catch (e) {
      clear(hist);
      hist.appendChild(h("p", { class: "error" }, e.message));
      return;
    }
    if (destroyed) return;
    const rows = res.peers || [];
    const open = q.get("ip") || "";
    const usage = rows.some((x) => x.bytes_in || x.bytes_out);
    const maxB = Math.max(1, ...rows.map((x) => Math.max(x.bytes_in || 0, x.bytes_out || 0)));
    const cols = [
      { key: "ip", label: "address", render: (x) => h("span", { class: "mono" }, (x.ip === open ? "\u25BE " : "\u25B8 ") + x.ip) },
      { key: "sessions", label: "sessions", num: true },
      { key: "connected_s", label: "time connected", num: true, render: (x) => h("span", { title: x.connected_more ? "A session was open before algodtop first looked, so at least this long" : null }, fmtDur(x.connected_s) + (x.connected_more ? "+" : "")) },
      { key: "reasons", label: "disconnect reasons", get: (x) => Object.keys(x.reasons || {}).join(","), render: (x) => reasonsCell(x.reasons), cls: "wrap" },
      { key: "resets", label: "resets", num: true },
      { key: "clusters", label: "clusters", get: (x) => (x.clusters || []).length, render: (x) => (x.clusters || []).map((id) => h("a", { href: "#cluster-" + id, class: "chip", on: { click: (e) => { e.preventDefault(); setParams({ cl: id }, { replace: true }); } } }, "#" + id)) },
      { key: "networks", label: "net", get: (x) => (x.networks || []).join(" ") },
      { key: "directions", label: "dir", get: (x) => (x.directions || []).join(" ") },
      { key: "last", label: "last seen", num: true, render: (x) => fmtShortTime(ns(x.last)) },
    ];
    if (usage) {
      cols.splice(5, 0,
        { key: "bytes_in", label: "received", num: true, render: (x) => inlineBar(x.bytes_in || 0, maxB, fmt(x.bytes_in || 0, "bytes"), "in") },
        { key: "bytes_out", label: "sent", num: true, render: (x) => inlineBar(x.bytes_out || 0, maxB, fmt(x.bytes_out || 0, "bytes"), "out") });
    }
    const after = (x) => {
      if (x.ip !== open) return null;
      const cell = h("td", { colspan: cols.length }, h("span", { class: "note" }, "Loading sessions..."));
      get("/api/sessions", { node: name, addr: x.ip, from: Math.floor(rg.from), to: Math.ceil(rg.to) }).then((sr) => {
        clear(cell);
        const ss = sr.sessions || [];
        cell.appendChild(sortableTable([
          { key: "start", label: "start", num: true, render: (z) => fmtDateTime(ns(z.start)) },
          { key: "end", label: "end", num: true, render: (z) => (z.end ? fmtDateTime(ns(z.end)) : "open") },
          { key: "dur", label: "length", num: true, get: (z) => (z.end ? z.end - z.start : Date.now() * 1e6 - z.start), render: (z) => fmtDur(((z.end || Date.now() * 1e6) - z.start) / 1e9) },
          { key: "direction", label: "dir", render: (z) => dirCell(z.direction) },
          { key: "network", label: "net" },
          { key: "reason", label: "reason" },
          { key: "bytes_in", label: "received", num: true, render: (z) => (z.bytes_in ? fmt(z.bytes_in, "bytes") : "") },
          { key: "bytes_out", label: "sent", num: true, render: (z) => (z.bytes_out ? fmt(z.bytes_out, "bytes") : "") },
          { key: "addr", label: "address as reported", cls: "mono" },
        ], ss, { sort: "start", desc: true, empty: "No sessions." }));
      }).catch((e) => {
        clear(cell);
        cell.appendChild(h("span", { class: "error" }, e.message));
      });
      return h("tr", null, cell);
    };
    clear(hist);
    hist.append(h("p", { class: "note" }, rows.length + " remote addresses in this range. Click a row for its sessions."),
      h("div", { class: "table-wrap", style: { maxHeight: "560px", overflowY: "auto" } }, sortableTable(cols, rows, {
        sort: "sessions", desc: true, limit: 1000, empty: "No peer sessions in this range.", after,
        rowClass: (x) => (x.ip === open ? "hl" : ""),
        onRow: (x, e) => {
          if (e.target.closest("a")) return;
          setParams({ ip: x.ip === open ? null : x.ip }, { replace: true });
        },
      })));
  }

  function clusterTimeline(c) {
    const W = 720, rowH = 6;
    const ss = (c.sessions || []).slice(0, 60);
    const t0 = Math.min(...ss.map((z) => ns(z.start)), ns(c.start) - 60e3);
    const t1 = ns(c.end) + 10e3;
    const H = 22 + ss.length * (rowH + 2);
    const x = linear(t0, t1, 40, W - 10);
    const svg = s("svg", { class: "plot", viewBox: "0 0 " + W + " " + H, width: "100%", height: H, preserveAspectRatio: "none" });
    svg.appendChild(s("rect", { x: x(ns(c.start)), y: 0, width: Math.max(2, x(ns(c.end)) - x(ns(c.start))), height: H - 14, class: "ann-range" }));
    const tt = timeTicks(t0, t1, 5);
    for (const t of tt.ticks) svg.appendChild(s("text", { x: x(t), y: H - 2, "text-anchor": "middle" }, tt.label(t)));
    const repeat = new Set(c.repeat_ips || []);
    ss.forEach((z, i) => {
      const y = 4 + i * (rowH + 2);
      const a = x(ns(z.start)), b = x(ns(z.end) || t1);
      const bar = s("rect", { x: a, y, width: Math.max(2, b - a), height: rowH, rx: 2 });
      bar.style.fill = repeat.has(z.ip) ? "var(--s6)" : "var(--in)";
      bar.addEventListener("pointermove", (e) => showTip(h("div", null,
        h("div", { class: "tt-time" }, fmtClock(ns(z.start), true) + " - " + fmtClock(ns(z.end), true)),
        h("div", null, z.ip + (repeat.has(z.ip) ? " (in more than one cluster)" : "")),
        h("div", { class: "muted" }, "reason: " + (z.reason || "?"))), e.clientX, e.clientY));
      bar.addEventListener("pointerleave", hideTip);
      svg.appendChild(bar);
    });
    return svg;
  }

  async function drawClusters() {
    const rg = rangeOf(q, { def: "24h" });
    const win = q.get("win") || "30s";
    const min = q.get("min") || "5";
    let res;
    try {
      res = await get("/api/peers/clusters", { node: name, from: Math.floor(rg.from), to: Math.ceil(rg.to), window: win, min });
    } catch (e) {
      clear(clus);
      clus.appendChild(h("p", { class: "error" }, e.message));
      return;
    }
    if (destroyed) return;
    const cl = res.clusters || [];
    const sel = parseInt(q.get("cl") || "0", 10);
    clear(clus);
    clus.appendChild(h("form", { class: "inline bar", on: { submit: (e) => {
      e.preventDefault();
      const f = e.target;
      setParams({ win: f.win.value || null, min: f.min.value || null }, { replace: true });
    } } },
    h("span", { class: "muted" }, "At least"), h("input", { type: "number", name: "min", value: min, min: 2, style: { width: "64px" } }),
    h("span", { class: "muted" }, "inbound sessions ending within"), h("input", { type: "text", name: "win", value: win, style: { width: "64px" } }),
    h("button", { type: "submit" }, "find")));
    if (!cl.length) {
      clus.appendChild(h("p", { class: "muted" }, "No clusters in this range."));
      return;
    }
    // Strip: one mark per cluster over the range, height by size.
    const W = 900, H = 50;
    const x = linear(rg.from, rg.to, 40, W - 10);
    const strip = s("svg", { class: "plot", viewBox: "0 0 " + W + " " + (H + 14), width: "100%", height: H + 14, preserveAspectRatio: "none" });
    const maxN = Math.max(...cl.map((c) => c.ips.length));
    const tt = timeTicks(rg.from, rg.to, 6);
    for (const t of tt.ticks) strip.appendChild(s("text", { x: x(t), y: H + 12, "text-anchor": "middle" }, tt.label(t)));
    for (const c of cl) {
      const hh = Math.max(4, (c.ips.length / maxN) * (H - 4));
      const m = s("rect", { x: x(ns(c.end)) - 2, y: H - hh, width: 4, height: hh, rx: 1 });
      m.style.fill = c.id === sel ? "var(--s6)" : c.repeat_ips && c.repeat_ips.length ? "var(--s1)" : "var(--in)";
      m.style.cursor = "pointer";
      m.addEventListener("click", () => setParams({ cl: c.id === sel ? null : c.id }, { replace: true }));
      m.addEventListener("pointermove", (e) => showTip(h("div", null, h("div", { class: "tt-time" }, fmtDateTime(ns(c.end))), "#" + c.id + ": " + c.ips.length + " peers"), e.clientX, e.clientY));
      m.addEventListener("pointerleave", hideTip);
      strip.appendChild(m);
    }
    clus.appendChild(h("div", { class: "lane" }, strip));
    const repeatAll = new Set(cl.flatMap((c) => c.repeat_ips || []));
    if (repeatAll.size) clus.appendChild(h("p", { class: "note" }, repeatAll.size + " addresses appear in more than one cluster: " + [...repeatAll].slice(0, 20).join(", ") + (repeatAll.size > 20 ? "..." : "")));
    for (const c of cl) {
      const open = c.id === sel;
      const det = h("details", { class: "section", open, id: "cluster-" + c.id },
        h("summary", null, "#" + c.id + "  " + fmtDateTime(ns(c.start)) + "  " + c.ips.length + " peers  ",
          h("span", { class: "note" }, Object.entries(c.reasons || {}).map(([k, v]) => k + " " + v).join(", ") + (c.repeat_ips && c.repeat_ips.length ? ", " + c.repeat_ips.length + " repeat" : ""))));
      det.addEventListener("toggle", () => {
        if (det.open && !det.dataset.drawn) fill();
      });
      const fill = () => {
        det.dataset.drawn = "1";
        const body = h("div", { class: "card" },
          clusterTimeline(c),
          h("dl", { class: "kvl" },
            h("dt", null, "addresses"), h("dd", { class: "mono" }, c.ips.map((ip) => h("span", { class: "chip", style: (c.repeat_ips || []).includes(ip) ? { outline: "1px solid var(--s6)" } : null }, ip))),
            h("dt", null, "window"), h("dd", null, fmtClock(ns(c.start), true) + " to " + fmtClock(ns(c.end), true)),
            c.traffic_in || c.traffic_out ? [h("dt", null, "member traffic"), h("dd", null, fmt(c.traffic_in, "bytes") + " received, " + fmt(c.traffic_out, "bytes") + " sent")] : null,
            Object.keys(c.protocols || {}).length ? [h("dt", null, "protocol sets"), h("dd", null, Object.entries(c.protocols).map(([k, v], i) => h("div", { class: "small" }, h("span", { class: "chip", style: { background: slot(i) , color: "#fff" } }, String(v)), " " + k)))] : null));
        det.appendChild(body);
      };
      if (open) fill();
      clus.appendChild(det);
      if (open) setTimeout(() => det.scrollIntoView({ block: "nearest" }), 0);
    }
  }

  drawHead();
  drawCurrent();
  drawHistory();
  drawClusters();
  timer = setInterval(drawCurrent, 30e3);

  return {
    update(r2) {
      const prev = q;
      q = r2.q;
      if (prev.get("f") !== q.get("f")) drawCurrent();
      const rk = ["r", "from", "to"];
      if (rk.some((k) => prev.get(k) !== q.get(k)) || prev.get("ip") !== q.get("ip")) drawHistory();
      if (rk.some((k) => prev.get(k) !== q.get(k)) || ["cl", "win", "min"].some((k) => prev.get(k) !== q.get(k))) drawClusters();
    },
    destroy() {
      destroyed = true;
      clearInterval(timer);
    },
  };
}
