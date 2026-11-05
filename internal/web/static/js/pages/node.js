// Node: a header strip of live values with their change vs 24 h ago, the
// range controls, a labelled event lane, and collapsible sections of
// synchronized charts. Range, open sections, full resolution and the
// events filter all live in the URL.

import { h, clear, sortableTable, inlineBar } from "../dom.js";
import { get, canWrite } from "../api.js";
import { rangeOf, setParams, encodeTime, decodeTime, listParam, nodeURL, navigate } from "../state.js";
import { fmt, fmtInt, fmtSeconds, fmtDelta, fmtShortTime, fmtDateTime, ns, isNum, shortAddr } from "../format.js";
import { healthBadge, healthReasons, hasProc, keyStatus, uptime, loadMarkers, toEv, notable, regionsFrom, rangeBar, nodePicker, annotateForm } from "../common.js";
import { Bus } from "../chart/crosshair.js";
import { Lane } from "../chart/lane.js";
import { Panel } from "../panel.js";
import { sections } from "../sections.js";

const DAY = 86400e3;

async function firstSample(node) {
  const to = Date.now();
  const r = await get("/api/series", { node, metric: "algodtop_up", from: to - 20 * 365 * DAY, to, points: 200 });
  const parts = r.parts || [];
  if (parts.length && parts[0].from > r.from) return parts[0].from / 1e6;
  const s = r.series && r.series[0];
  if (s) for (let i = 0; i < s.avg.length; i++) if (s.avg[i] !== null) return s.t[i] / 1e6;
  return to - DAY;
}

// valueAgo reads a metric's average in a 10 minute window 24 h ago, summed
// over its series.
async function valueAgo(node, metric, fn) {
  const at = Date.now() - DAY;
  try {
    const r = await get("/api/series", { node, metric, fn, from: at - 5 * 60e3, to: at + 5 * 60e3, points: 1 });
    let sum = null;
    for (const s of r.series || []) {
      const v = s.avg && s.avg[0];
      if (v !== null && v !== undefined) sum = (sum || 0) + v;
    }
    return sum;
  } catch (e) {
    return null;
  }
}

export function mount(root, app, r) {
  const name = r.node;
  let q = r.q;
  const bus = new Bus();
  const ctx = { events: [], allEvents: [], annotations: [], regions: [], onEventClick: (ev) => focusEvent(ev) };
  const page = { bus, ctx, relative: false, templateRank: new Map() };
  const panels = [];
  let names = null; // stored metric names on this node
  let first = 0; // first stored sample, for "all"
  let ago = {};
  let timers = [];
  let view = null;
  let destroyed = false;

  const stripEl = h("div", { class: "strip" });
  const headEl = h("div", { class: "page-head" });
  const rangeEl = h("div");
  const formSlot = h("div");
  const lane = new Lane({ bus, ctx, axis: true });
  const secRoot = h("div");
  const pageEl = h("div", { id: "node" }, headEl, stripEl, rangeEl, formSlot, lane.el, secRoot);
  root.appendChild(pageEl);

  const node = () => app.byName(name);

  function drawHead() {
    clear(headEl);
    headEl.append(h("h1", null, name),
      nodePicker(app.nodes, name, (v) => v && navigate(nodeURL(v, location.search))),
      h("a", { href: "/peers/" + encodeURIComponent(name) }, "peers"),
      h("a", { href: "/events?node=" + encodeURIComponent(name) }, "events"),
      h("a", { href: "/compare?node=" + encodeURIComponent(name) }, "compare"),
      h("a", { href: "/explorer?node=" + encodeURIComponent(name) }, "explore metrics"));
  }

  function stat(k, v, d, cls) {
    return h("div", { class: "stat " + (cls || "") }, h("div", { class: "k" }, k), h("div", { class: "v" }, v), d !== undefined ? h("div", { class: "d" }, d) : null);
  }

  // delta compares with 24 h ago. A node with less history than that gets
  // an empty line, which keeps the cards one height.
  function delta(cur, old, unit, label) {
    if (!isNum(cur) || old === null || old === undefined) return label || "";
    return fmtDelta(cur - old, unit) + " vs 24 h ago";
  }

  function drawStrip() {
    const n = node();
    clear(stripEl);
    if (!n) {
      stripEl.appendChild(h("div", { class: "banner warn" }, "No node named " + name + " is configured."));
      return;
    }
    const p = n.peers || {};
    const total = (p.ws_in || 0) + (p.ws_out || 0) + (p.p2p_in || 0) + (p.p2p_out || 0);
    stripEl.append(...[
      stat("health", healthBadge(n), healthReasons(n) || "no problems found", "wide"),
      stat("round", n.round ? fmtInt(n.round) : "-", isNum(ago.round) && n.round ? "+" + fmtInt(n.round - ago.round) + " rounds in 24 h" : ""),
      stat("lag", n.up && isNum(n.lag) ? fmtSeconds(n.lag) : "-", delta(n.lag, ago.lag, "s")),
      stat("round time", n.round_time ? fmtSeconds(n.round_time) : "-", delta(n.round_time, ago.rtime, "s")),
      stat("peers", String(total), "WS " + (p.ws_in || 0) + "/" + (p.ws_out || 0) + "  P2P " + (p.p2p_in || 0) + "/" + (p.p2p_out || 0) + (isNum(ago.peers) ? ", " + fmtDelta(total - ago.peers, "") + " vs 24 h" : "")),
      hasProc(n) ? stat("CPU", isNum(n.cpu) && n.up ? n.cpu.toFixed(1) + "%" : "-", delta(n.cpu, ago.cpu, "%")) : null,
      hasProc(n) ? stat("RSS", n.rss && n.up ? fmt(n.rss, "bytes") : "-", delta(n.rss, ago.rss, "bytes")) : null,
      stat("version", n.version || "-", n.branch || n.network || ""),
      stat("uptime", n.up ? uptime(n) : "down", n.start_time ? "since " + fmtShortTime(ns(n.start_time)) : ""),
      stat("last effective config change", n.config_changed ? fmtShortTime(ns(n.config_changed.time)) : "none seen", n.config_changed ? n.config_changed.msg : "", "wide")].filter(Boolean));
    if (n.catchup) stripEl.appendChild(stat("catchup", "running", "the node is catching up"));
  }

  async function loadAgo() {
    const [round, lag, rtime, peers, cpu, rss] = await Promise.all([
      valueAgo(name, "algodtop_round"), valueAgo(name, "algodtop_sync_lag_seconds"), valueAgo(name, "algodtop_round_time_seconds"),
      valueAgo(name, "algodtop_peers"), valueAgo(name, "proc_cpu_seconds_total", "rate"), valueAgo(name, "proc_resident_bytes")]);
    ago = { round, lag, rtime, peers, cpu: isNum(cpu) ? cpu * 100 : null, rss };
    if (!destroyed) drawStrip();
  }

  function currentView() {
    const rg = rangeOf(q, { def: "1h", node: node(), first });
    const raw = q.get("raw") === "1";
    return { node: name, from: rg.from, to: rg.to, live: rg.live, raw, key: (rg.live ? rg.key : rg.from + "-" + rg.to) + (raw ? "/raw" : "") };
  }

  function drawRange() {
    clear(rangeEl);
    rangeEl.appendChild(rangeBar(q, { node: node(), annotate: (a, b) => showAnnotate(a, b) }));
  }

  function showAnnotate(a, b) {
    clear(formSlot);
    formSlot.appendChild(annotateForm(name, a, b, () => {
      clear(formSlot);
      refreshMarkers();
    }));
  }

  // Sections.
  const closed = new Set(listParam(q, "closed"));
  const io = new IntersectionObserver((entries) => {
    for (const e of entries) {
      const p = e.target.__panel;
      if (!p) continue;
      p.visible = e.isIntersecting;
      if (p.visible && view && p.loadedKey !== view.key && !p.loading) p.load(view);
    }
  }, { rootMargin: "200px" });

  const evTableSlot = h("div");
  const tplSlot = h("div");
  const warnSlot = h("div");
  const partSlot = h("div");
  const usageSlot = h("div");

  function buildSections() {
    clear(secRoot);
    for (const p of panels) {
      io.unobserve(p.el);
      p.destroy();
    }
    panels.length = 0;
    const n = node();
    const defs = sections(page, n);
    for (const sec of defs) {
      const grid = h("div", { class: "grid" });
      const specs = sec.panels.filter((spec) => {
        const need = spec.metrics || (spec.q || []).map((x) => x.metric);
        if (spec.usage && !(n && n.peer_usage)) return false;
        // Only a local node or an agent reads node.log.
        if (spec.logs && !hasProc(n)) return false;
        return spec.always || !need.length || !names || need.some((m) => names.has(m));
      });
      // A context chart only helps read the charts beside it.
      const shown = specs.filter((s) => !s.context).length;
      for (const spec of shown ? specs : []) {
        const p = new Panel(spec, page);
        p.el.__panel = p;
        panels.push(p);
        grid.appendChild(p.el);
        io.observe(p.el);
      }
      const det = h("details", { class: "section", open: !closed.has(sec.id), dataset: { id: sec.id } },
        h("summary", null, sec.title, h("span", { class: "note" }, shown ? shown + (shown === 1 ? " chart" : " charts") : "no data on this node")), grid);
      if (sec.id === "bandwidth") det.appendChild(usageSlot);
      if (sec.id === "logs") det.append(h("div", { class: "row", style: { marginTop: "12px" } }, h("div", { class: "col" }, tplSlot), h("div", { class: "col" }, warnSlot)));
      det.addEventListener("toggle", () => {
        if (det.open) closed.delete(sec.id);
        else closed.add(sec.id);
        setParams({ closed: [...closed].join(",") || null }, { replace: true, silent: true });
        // Tables below the charts load when their section first opens.
        if (det.open && sec.id === "logs" && !tplSlot.firstChild) drawLogs();
        if (det.open && sec.id === "bandwidth" && !usageSlot.firstChild) drawUsage();
      });
      secRoot.appendChild(det);
    }
    const partDet = h("details", { class: "section", open: !closed.has("participation"), dataset: { id: "participation" } }, h("summary", null, "Participation"), partSlot);
    partDet.addEventListener("toggle", () => {
      if (partDet.open) closed.delete("participation");
      else closed.add("participation");
      setParams({ closed: [...closed].join(",") || null }, { replace: true, silent: true });
    });
    secRoot.appendChild(partDet);
    const evDet = h("details", { class: "section", open: !closed.has("events"), id: "node-events" }, h("summary", null, "Events"), evTableSlot);
    evDet.addEventListener("toggle", () => {
      if (evDet.open) closed.delete("events");
      else closed.add("events");
      setParams({ closed: [...closed].join(",") || null }, { replace: true, silent: true });
    });
    secRoot.appendChild(evDet);
  }

  function drawParticipation() {
    const n = node();
    clear(partSlot);
    if (!n) return;
    const keys = n.keys || [], accts = n.accounts || [];
    if (!keys.length && !accts.length) {
      partSlot.appendChild(h("p", { class: "muted" }, "No participation keys are installed and no accounts are listed for this node."));
      return;
    }
    if (keys.length) {
      partSlot.appendChild(h("div", { class: "table-wrap" }, sortableTable([
        { key: "address", label: "account", render: (k) => h("span", { class: "mono", title: k.address }, shortAddr(k.address)) },
        { key: "status", label: "status", get: (k) => keyStatus(k, n.round), title: "active is the key the account votes with; superseded keys end before it and can be deleted" },
        { key: "first", label: "first round", num: true, render: (k) => fmtInt(k.first) },
        { key: "last", label: "last round", num: true, render: (k) => fmtInt(k.last) },
        { key: "rounds_left", label: "rounds left", num: true, render: (k) => fmtInt(k.rounds_left) },
        { key: "days_left", label: "days left", num: true, render: (k) => (isNum(k.days_left) ? k.days_left.toFixed(1) : "-") },
        { key: "last_vote", label: "last vote", num: true, render: (k) => (k.last_vote ? fmtInt(k.last_vote) : "never") },
        { key: "last_proposal", label: "last proposal", num: true, render: (k) => (k.last_proposal ? fmtInt(k.last_proposal) : "never") },
      ], keys, { sort: "days_left", desc: false, rowClass: (k) => (k.superseded || (n.round && k.last < n.round) ? "muted" : null) })));
    }
    if (accts.length) {
      partSlot.appendChild(h("div", { class: "table-wrap", style: { marginTop: "8px" } }, sortableTable([
        { key: "address", label: "account", render: (a) => h("span", { class: "mono", title: a.address }, shortAddr(a.address)) },
        { key: "status", label: "status", get: (a) => (a.status === "Offline" && a.vote_key ? "suspended" : a.status) },
        { key: "key_matches", label: "installed key is the vote key", render: (a) => (a.key_matches ? "yes" : "no") },
        { key: "incentive_eligible", label: "incentive eligible", render: (a) => (a.incentive_eligible ? "yes" : "no") },
        { key: "last_proposed", label: "last proposed", num: true, render: (a) => fmtInt(a.last_proposed) },
        { key: "last_heartbeat", label: "last heartbeat", num: true, render: (a) => fmtInt(a.last_heartbeat) },
        { key: "rounds_since_vote", label: "rounds since vote", num: true, render: (a) => fmtInt(a.rounds_since_vote) },
        { key: "expected_interval", label: "expected vote interval", num: true, render: (a) => (isNum(a.expected_interval) ? fmtInt(a.expected_interval) + " rounds" : "-") },
      ], accts)));
    }
  }

  // Events table, filtered by a marker click (ev=kind) and highlighting the
  // event nearest the clicked time (at=).
  function drawEvents() {
    clear(evTableSlot);
    const kind = q.get("ev") || "";
    const at = decodeTime(q.get("at"));
    const all = ctx.allEvents.slice().sort((a, b) => b.t - a.t);
    const kinds = [...new Set(all.map((e) => e.kind))].sort();
    const bar = h("div", { class: "bar" },
      h("span", { class: "muted" }, "kind"),
      h("select", { on: { change: (e) => setParams({ ev: e.target.value || null, at: null }, { replace: true }) } },
        h("option", { value: "" }, "all (" + all.length + ")"),
        kinds.map((k) => h("option", { value: k, selected: k === kind }, k + " (" + all.filter((e) => e.kind === k).length + ")"))),
      kind ? h("button", { type: "button", class: "small", on: { click: () => setParams({ ev: null, at: null }, { replace: true }) } }, "clear filter") : null,
      h("span", { class: "grow" }),
      canWrite() ? h("button", { type: "button", class: "small", on: { click: () => showAnnotate(Date.now(), 0) } }, "add annotation now") : null);
    evTableSlot.appendChild(bar);
    const rows = kind ? all.filter((e) => e.kind === kind) : all;
    let hl = null;
    if (at) {
      let bd = Infinity;
      for (const e of rows) {
        const d = Math.abs(e.t - at);
        if (d < bd) {
          bd = d;
          hl = e;
        }
      }
    }
    const tbl = sortableTable([
      { key: "t", label: "time", num: true, render: (e) => fmtDateTime(e.t) },
      { key: "kind", label: "kind", render: (e) => h("span", { class: "sev " + e.sev }, e.kind) },
      { key: "sev", label: "severity" },
      { key: "msg", label: "message", cls: "wrap" },
    ], rows, { sort: "t", desc: true, limit: 500, rowClass: (e) => (e === hl ? "hl" : ""), empty: "No events in this range." });
    evTableSlot.appendChild(h("div", { class: "table-wrap", style: { maxHeight: "420px", overflowY: "auto" } }, tbl));
    if (rows.length > 500) evTableSlot.appendChild(h("p", { class: "note" }, "Showing the newest 500 of " + rows.length + ". The Events page has every event."));
    if (hl) {
      const tr = tbl.querySelector("tr.hl");
      if (tr) setTimeout(() => tr.scrollIntoView({ block: "nearest" }), 0);
    }
  }

  function focusEvent(ev) {
    setParams({ ev: ev.kind, at: encodeTime(ev.t) }, { replace: true, silent: true });
    q = new URLSearchParams(location.search);
    const det = document.getElementById("node-events");
    if (det) det.open = true;
    drawEvents();
    if (det) det.scrollIntoView({ behavior: "smooth", block: "start" });
  }

  // Warnings and errors from node.log, and template counts with inline bars.
  async function drawLogs() {
    if (closed.has("logs") || !view) return;
    const errs = ctx.allEvents.filter((e) => e.kind === "log-warn" || e.kind === "log-error").sort((a, b) => b.t - a.t);
    clear(warnSlot);
    warnSlot.append(h("h3", null, "Warnings and errors"), h("div", { class: "table-wrap", style: { maxHeight: "360px", overflowY: "auto", marginTop: "6px" } }, sortableTable([
      { key: "t", label: "time", num: true, render: (e) => fmtShortTime(e.t) },
      { key: "sev", label: "level", render: (e) => h("span", { class: "sev " + e.sev }, e.sev) },
      { key: "msg", label: "message", cls: "wrap" },
    ], errs, { sort: "t", desc: true, limit: 300, empty: "No warnings or errors in this range." })));
    try {
      // Totals come from the same series as the heatmap, at bucket width
      // fine enough to stay on the raw tier where it can; rolled-up tiers
      // undercount increase (see /api/logs/templates).
      const r = await get("/api/series", { node: name, metric: "log_template_lines_total", fn: "increase", from: Math.floor(view.from), to: Math.ceil(view.to), points: 240, limit: 2000 });
      const list = (r.series || []).map((sr) => ({
        level: sr.labels.level || "", template: sr.labels.template || "",
        count: sr.avg.reduce((a, v) => a + (v === null ? 0 : v), 0),
      })).filter((t) => t.count > 0).sort((a, b) => b.count - a.count).map((t, i) => Object.assign({ rank: i + 1 }, t));
      page.templateRank = new Map(list.map((t) => [t.level + "\u0000" + t.template, t.rank]));
      const max = list.length ? list[0].count : 0;
      const lines = h("div");
      clear(tplSlot);
      tplSlot.append(h("h3", null, "Template counts"), h("div", { class: "table-wrap", style: { maxHeight: "360px", overflowY: "auto", marginTop: "6px" } }, sortableTable([
        { key: "rank", label: "#", num: true },
        { key: "level", label: "level" },
        { key: "template", label: "template", cls: "wrap mono" },
        { key: "count", label: "lines", num: true, render: (t) => inlineBar(t.count, max, fmtInt(t.count)) },
      ], list, { sort: "rank", desc: false, limit: 300, empty: "No log lines in this range.",
        onRow: async (t) => {
          clear(lines);
          lines.appendChild(h("p", { class: "note" }, "Loading kept lines..."));
          try {
            const lr = await get("/api/logs/lines", { node: name, template: t.template, from: Math.floor(view.from), to: Math.ceil(view.to), limit: 50 });
            clear(lines);
            const ls = lr.lines || [];
            lines.appendChild(h("p", { class: "note" }, ls.length ? "Kept raw lines for #" + t.rank + " (warnings and errors are kept; info lines only when configured):" : "No raw lines are kept for #" + t.rank + "."));
            for (const l of ls) lines.appendChild(h("pre", { class: "mono", style: { whiteSpace: "pre-wrap", margin: "2px 0" } }, l.line || l.Line || JSON.stringify(l)));
          } catch (e) {
            clear(lines);
            lines.appendChild(h("p", { class: "error" }, e.message));
          }
        } })), lines);
      const heat = panels.find((p) => p.spec.id === "log-heat");
      if (heat && heat.chart.data) heat.load(view);
    } catch (e) {
      clear(tplSlot);
      tplSlot.appendChild(h("p", { class: "error" }, e.message));
    }
  }

  // Top talkers from per-peer usage.
  async function drawUsage() {
    clear(usageSlot);
    const n = node();
    if (!n || !n.peer_usage || !view || closed.has("bandwidth")) return;
    try {
      const r = await get("/api/peers/usage", { node: name, from: Math.floor(view.from), to: Math.ceil(view.to), points: 10, top: 5 });
      const top = (r.top || []).slice(0, 20);
      const max = Math.max(1, ...top.map((t) => Math.max(t.rate_in, t.rate_out)));
      usageSlot.append(h("h3", { style: { margin: "12px 0 6px" } }, "Top talkers"), h("div", { class: "table-wrap" }, sortableTable([
        { key: "addr", label: "peer", render: (t) => h("span", { class: "mono" }, t.addr || t.key) },
        { key: "network", label: "net" },
        { key: "rate_in", label: "received", num: true, render: (t) => inlineBar(t.rate_in, max, fmt(t.rate_in, "B/s"), "in") },
        { key: "rate_out", label: "sent", num: true, render: (t) => inlineBar(t.rate_out, max, fmt(t.rate_out, "B/s"), "out") },
        { key: "in", label: "total received", num: true, render: (t) => fmt(t.in, "bytes") },
        { key: "out", label: "total sent", num: true, render: (t) => fmt(t.out, "bytes") },
      ], top, { sort: "rate_in", desc: true, empty: "No per-peer usage in this range." })));
    } catch (e) {
      usageSlot.appendChild(h("p", { class: "error" }, e.message));
    }
  }

  async function refreshMarkers() {
    if (!view) return;
    try {
      await loadMarkers(ctx, name, view.from, view.to);
    } catch (e) {
      return;
    }
    if (destroyed) return;
    lane.setRange(view.from, view.to);
    for (const p of panels) if (p.chart.data) p.chart.render();
    drawEvents();
  }

  async function reload() {
    view = currentView();
    if (q.get("r") === "all" && !first) {
      try {
        first = await firstSample(name);
      } catch (e) {
        first = Date.now() - DAY;
      }
      view = currentView();
    }
    drawRange();
    lane.setRange(view.from, view.to);
    await refreshMarkers();
    for (const p of panels) {
      if (p.visible) p.load(view);
    }
    drawLogs();
    drawUsage();
  }

  // Live: refetch the newest part of visible charts.
  function tick() {
    if (!view || !view.live || destroyed) return;
    view = currentView();
    lane.setRange(view.from, view.to);
    const now = Date.now();
    for (const p of panels) {
      if (!p.visible || p.loading) continue;
      if (p.loadedKey !== view.key) p.load(view);
      else if (now - p.lastLoad >= p.refreshInterval()) p.load(view, { tail: true });
    }
  }

  bus.onZoom((a, b) => setParams({ from: encodeTime(a), to: encodeTime(b), r: null }));

  drawHead();
  drawStrip();
  drawParticipation();
  (async () => {
    try {
      const m = await get("/api/metrics", { node: name });
      names = new Set((m.metrics || []).map((x) => x.name));
    } catch (e) {
      names = null;
    }
    if (destroyed) return;
    buildSections();
    reload();
  })();
  loadAgo();
  timers.push(setInterval(tick, 5000));
  timers.push(setInterval(() => view && view.live && refreshMarkers(), 60e3));
  timers.push(setInterval(loadAgo, 10 * 60e3));

  let lastStrip = 0;
  return {
    update(r2) {
      const prev = q;
      q = r2.q;
      const keys = ["r", "from", "to", "raw"];
      const changed = keys.some((k) => prev.get(k) !== q.get(k));
      if (changed) reload();
      else {
        drawEvents();
        drawRange();
      }
    },
    status() {
      const now = Date.now();
      if (now - lastStrip < 2000) return;
      lastStrip = now;
      drawStrip();
    },
    event(e) {
      if (e.node && e.node !== name) return;
      const ev = toEv(e);
      if (!view || ev.t < view.from) return;
      ctx.allEvents.push(ev);
      if (notable([ev]).length) {
        ctx.events.push(ev);
        ctx.regions = regionsFrom(ctx.allEvents, name, Date.now());
        lane.render();
        for (const p of panels) if (p.chart.data) p.chart.render();
      }
    },
    theme() {
      for (const p of panels) if (p.spec.type === "heatmap" && p.chart.data) p.chart.render();
    },
    destroy() {
      destroyed = true;
      timers.forEach(clearInterval);
      io.disconnect();
      lane.destroy();
      for (const p of panels) p.destroy();
    },
  };
}

