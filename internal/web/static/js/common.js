// Pieces several pages share: health badges, participation status, the
// range bar, node pickers, and turning events into chart markers and
// down-time regions.

import { h } from "./dom.js";
import { get, canWrite, send } from "./api.js";
import { PRESETS, setParams, encodeTime, rangeOf, parseDur } from "./state.js";
import { fmtDur, toLocalInput, ns, shortAddr } from "./format.js";

export function healthLevel(n) {
  if (!n) return "down";
  if (!n.up) return "down";
  const l = n.health && n.health.level;
  return l === "critical" || l === "warn" ? l : "ok";
}

const HEALTH_WORD = { ok: "OK", warn: "WARN", critical: "CRIT", down: "DOWN" };

export function healthBadge(n) {
  const l = healthLevel(n);
  return h("span", { class: "health " + l, title: n && n.summary ? n.summary : "" }, HEALTH_WORD[l]);
}

// healthReason is the first reason in words, or the summary.
export function healthReason(n) {
  if (!n) return "";
  const rs = (n.health && n.health.reasons) || [];
  if (rs.length) return rs.map((r) => r.text).join("; ");
  if (!n.up && n.last_error) return n.last_error;
  return n.summary && n.summary !== "OK" ? n.summary : "";
}

// expiryKeys returns one key per account: the key it votes with, or else
// the one that lasts longest. A renewed account keeps its old key
// installed, and that key's expiry does not matter.
export function expiryKeys(keys) {
  const by = new Map();
  for (const k of keys || []) {
    const o = by.get(k.address);
    if (!o || k.active || (!o.active && k.last > o.last)) by.set(k.address, k);
  }
  return [...by.values()];
}

// keyStatus describes one installed key in a word or two.
export function keyStatus(k, round) {
  if (k.active) return "active";
  if (k.superseded) return "superseded";
  if (round && k.last < round) return "expired";
  if (round && k.first > round) return "not valid yet";
  return "installed";
}

// healthReasons lists each reason on its own line, worst first, or
// returns null when there are none. Each line's mark shows its level, so
// a warning under a critical node reads as a warning.
export function healthReasons(n) {
  if (!n) return null;
  const rs = ((n.health && n.health.reasons) || []).slice();
  if (!rs.length) {
    const t = healthReason(n);
    return t ? h("ul", { class: "reasons" }, h("li", { class: n.up ? "warn" : "critical" }, t)) : null;
  }
  const rank = { critical: 2, warn: 1 };
  rs.sort((a, b) => (rank[b.level] || 0) - (rank[a.level] || 0));
  return h("ul", { class: "reasons" }, rs.map((r) => h("li", { class: r.level }, r.text)));
}

// participation summarizes keys and accounts in a few words.
export function participation(n) {
  const keys = n.keys || [];
  const accts = n.accounts || [];
  if (!keys.length && !accts.length) return "not participating";
  const parts = [];
  for (const a of accts) {
    let st = a.status || "?";
    if (st === "Offline" && a.vote_key) st = "suspended";
    parts.push(shortAddr(a.address) + " " + st.toLowerCase() + (a.incentive_eligible ? ", eligible" : ""));
  }
  if (keys.length) {
    const exp = expiryKeys(keys);
    const min = Math.min(...exp.map((k) => (Number.isFinite(k.days_left) ? k.days_left : Infinity)));
    const n = exp.length;
    let s = n + (n === 1 ? " account" : " accounts");
    if (keys.length !== n) s += ", " + keys.length + " keys";
    if (Number.isFinite(min)) s += min <= 0 ? ", a key has expired" : ", next expiry in " + fmtDays(min);
    parts.push(s);
  }
  return parts.join("; ");
}

function fmtDays(d) {
  if (d < 1) return Math.round(d * 24) + " h";
  return d.toFixed(d < 10 ? 1 : 0) + " d";
}

// hasProc reports whether algodtop reads the node's process stats and
// node.log. A REST or no-install SSH node has neither: an agent or a local
// node has both.
export function hasProc(n) {
  return !!n && n.kind !== "rest" && n.kind !== "ssh";
}

export function uptime(n) {
  const st = ns(n.start_time) || ns(n.up_since);
  return st ? fmtDur((Date.now() - st) / 1000) : "-";
}

// Events that do not draw as markers. Connects, disconnects and log
// warnings are too frequent; they show in their own charts and tables
// instead. An alert's hourly reminder would draw a line an hour across
// every chart, while its firing and clearing already mark the stretch.
const QUIET = new Set(["connect", "disconnect", "log-warn", "annotation", "alert-repeated"]);

export function toEv(e) {
  return { t: ns(e.time), kind: e.kind, sev: e.severity || "info", msg: e.msg, node: e.node, attrs: e.attrs || {}, raw: e };
}

export function notable(evs) {
  return evs.filter((e) => !QUIET.has(e.kind));
}

// regionsFrom finds "algodtop was down" (monitor-gap) and "algod was down"
// (node-down until node-up) stretches.
export function regionsFrom(evs, node, now) {
  const out = [];
  const sorted = evs.slice().sort((a, b) => a.t - b.t);
  const downAt = new Map();
  for (const e of sorted) {
    if (e.kind === "monitor-gap") {
      const a = ns(e.attrs.from), b = ns(e.attrs.to) || e.t;
      if (a && b > a) out.push({ from: a, to: b, type: "monitor" });
    } else if (e.kind === "node-down") {
      if (!downAt.has(e.node)) downAt.set(e.node, e.t);
    } else if (e.kind === "node-up") {
      const a = downAt.get(e.node) || ns(e.attrs.down_since);
      downAt.delete(e.node);
      if (a && e.t > a) out.push({ from: a, to: e.t, type: "algod" });
    }
  }
  for (const [n, a] of downAt) {
    if (!node || node === n) out.push({ from: a, to: now || Date.now(), type: "algod" });
  }
  return out;
}

// fetchEvents gets the events of a node and range, only of kinds when
// given. The server keeps the newest 5000. On a busy range peer churn and
// log lines fill that cap, so when it cuts the answer short, a second
// request fetches the other kinds on their own. Then only the oldest quiet
// events are missing. counts is the server's count of every kind in the
// range, and total is the count for the kinds asked.
export async function fetchEvents(node, from, to, kinds) {
  const p = { node: node || "", from: Math.floor(from), to: Math.ceil(to), limit: 5000 };
  if (kinds && kinds.length) p.kind = kinds.join(",");
  const r = await get("/api/events", p);
  const counts = r.counts || {};
  const want = (k) => !p.kind || kinds.includes(k);
  const total = Object.keys(counts).filter(want).reduce((a, k) => a + counts[k], 0);
  let events = (r.events || []).map(toEv);
  if (events.length < total) {
    const rest = Object.keys(counts).filter((k) => want(k) && !QUIET.has(k));
    if (rest.length) {
      const r2 = await get("/api/events", Object.assign({}, p, { kind: rest.join(",") }));
      const got = new Set(rest);
      events = events.filter((e) => !got.has(e.kind)).concat((r2.events || []).map(toEv)).sort((a, b) => a.t - b.t);
    }
  }
  const annotations = (r.annotations || []).map((a) => Object.assign({}, a, { start: ns(a.start), end: ns(a.end) }));
  return { events, counts, total, annotations, silences: r.silences || [] };
}

// loadMarkers fetches events and annotations for a node and range and
// fills a page context for the charts.
export async function loadMarkers(ctx, node, from, to) {
  const r = await fetchEvents(node, from - 3600e3, to);
  const evs = r.events;
  ctx.allEvents = evs.filter((e) => e.t >= from);
  ctx.events = notable(ctx.allEvents);
  ctx.annotations = r.annotations;
  ctx.silences = r.silences;
  ctx.regions = regionsFrom(evs, node, Date.now());
  return ctx;
}

// rangeBar renders the time-range controls. It writes the URL; the page
// re-renders from it.
export function rangeBar(q, opts = {}) {
  const cur = q.get("from") ? "custom" : q.get("r") || opts.def || "1h";
  const bar = h("div", { class: "bar range" });
  const seg = h("div", { class: "seg" });
  for (const p of PRESETS) {
    seg.appendChild(h("button", { type: "button", class: cur === p ? "on" : "", on: { click: () => setParams({ r: p, from: null, to: null }) } }, p));
  }
  bar.appendChild(seg);
  if (opts.restart !== false) {
    bar.appendChild(h("button", { type: "button", class: cur === "restart" ? "on" : "", title: "From the last algod start", on: { click: () => setParams({ r: "restart", from: null, to: null }) } }, "since last restart"));
  }
  const rg = rangeOf(q, { def: opts.def, node: opts.node });
  const fi = h("input", { type: "datetime-local", value: toLocalInput(rg.from), "aria-label": "From" });
  const ti = h("input", { type: "datetime-local", value: toLocalInput(rg.to), "aria-label": "To" });
  const apply = () => {
    const a = Date.parse(fi.value), b = Date.parse(ti.value);
    if (a && b && b > a) setParams({ from: encodeTime(a), to: encodeTime(b), r: null });
  };
  bar.appendChild(h("span", { class: "nowrap" + (cur === "custom" ? " on" : "") }, fi, " to ", ti, " ", h("button", { type: "button", class: cur === "custom" ? "on" : "", on: { click: apply } }, "custom")));
  if (opts.raw !== false) {
    const span = rg.to - rg.from;
    const tooLong = span > parseDur("30d");
    bar.appendChild(h("label", { class: "check", title: tooLong ? "Full resolution covers the raw tier, the last 30 days" : "Ask for every stored sample (tier=raw)" },
      h("input", { type: "checkbox", checked: q.get("raw") === "1", disabled: tooLong, on: { change: (e) => setParams({ raw: e.target.checked ? "1" : null }) } }), "full resolution"));
  }
  if (cur === "custom" && opts.annotate && canWrite()) {
    bar.appendChild(h("button", { type: "button", title: "Add an annotation over this range", on: { click: () => opts.annotate(rg.from, rg.to) } }, "annotate this range"));
  }
  bar.appendChild(h("span", { class: "grow" }));
  bar.appendChild(h("span", { class: "live-dot" + (rg.live ? " on" : ""), title: rg.live ? "The range follows the clock" : "A fixed range" }, rg.live ? "live" : "fixed range"));
  return bar;
}

export function nodePicker(nodes, cur, onPick, opts = {}) {
  const sel = h("select", { "aria-label": opts.label || "Node", on: { change: (e) => onPick(e.target.value) } });
  if (opts.any) sel.appendChild(h("option", { value: "" }, opts.any));
  for (const n of nodes) sel.appendChild(h("option", { value: n.name, selected: n.name === cur }, n.name));
  return sel;
}

// annotateDialog is a small inline form for a new annotation over a range.
export function annotateForm(node, from, to, done) {
  const text = h("input", { type: "text", placeholder: "What happened", style: { minWidth: "260px" } });
  const sil = h("input", { type: "text", placeholder: "silence for, e.g. 10m", style: { width: "150px" } });
  const msg = h("span", { class: "note" });
  const form = h("form", { class: "inline banner", on: {
    submit: async (e) => {
      e.preventDefault();
      if (!text.value.trim()) return;
      try {
        const body = { text: text.value.trim(), node: node || "", start: Math.round(from * 1e6), end: to ? Math.round(to * 1e6) : 0 };
        if (sil.value.trim()) body.silence_for = sil.value.trim();
        await send("POST", "/api/annotations", body);
        msg.textContent = "Saved.";
        if (done) done();
      } catch (err) {
        msg.textContent = err.message;
        msg.className = "error";
      }
    },
  } }, h("span", { class: "muted" }, "Annotate " + (node || "all nodes") + ":"), text, sil, h("button", { type: "submit", class: "primary" }, "Save"), msg);
  return form;
}
