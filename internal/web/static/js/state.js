// View state lives in the URL, so any view can be bookmarked or pasted
// into a chat. The path names the page and node; the query holds the rest.

import { ns } from "./format.js";

export const PRESETS = ["1h", "6h", "24h", "7d", "30d", "1y", "all"];

const DUR = { s: 1e3, m: 60e3, h: 3600e3, d: 86400e3, w: 7 * 86400e3, y: 365 * 86400e3 };

// parseDur reads "90s", "15m", "6h", "7d" or "1y" as milliseconds.
export function parseDur(s) {
  const m = /^(\d+(?:\.\d+)?)([smhdwy])$/.exec(String(s || "").trim());
  return m ? parseFloat(m[1]) * DUR[m[2]] : 0;
}

export function durName(ms) {
  for (const [u, n] of [["y", DUR.y], ["d", DUR.d], ["h", DUR.h], ["m", DUR.m]]) {
    if (ms >= n && ms % n === 0) return ms / n + u;
  }
  return Math.round(ms / 1000) + "s";
}

// encodeTime writes a time for the URL as UTC ISO without milliseconds.
export function encodeTime(ms) {
  return new Date(Math.round(ms / 1000) * 1000).toISOString().replace(/\.000Z$/, "Z");
}

export function decodeTime(v) {
  if (!v) return 0;
  if (/^\d+$/.test(v)) {
    const n = parseInt(v, 10);
    return n > 1e17 ? n / 1e6 : n > 1e11 ? n : n * 1000;
  }
  const t = Date.parse(v);
  return Number.isFinite(t) ? t : 0;
}

export function route() {
  const parts = location.pathname.split("/").filter(Boolean).map(decodeURIComponent);
  const page = parts[0] || "fleet";
  return { page, node: parts.slice(1).join("/"), q: new URLSearchParams(location.search) };
}

let onChange = () => {};
export function setRouteHandler(fn) {
  onChange = fn;
}

// navigate changes the URL and re-renders. replace keeps history short for
// small changes such as opening a section.
export function navigate(url, opts = {}) {
  if (url === location.pathname + location.search) {
    if (!opts.silent) onChange();
    return;
  }
  if (opts.replace) history.replaceState(null, "", url);
  else history.pushState(null, "", url);
  if (!opts.silent) onChange();
}

// setParams merges params into the current query. null deletes a key.
export function setParams(params, opts = {}) {
  const q = new URLSearchParams(location.search);
  for (const [k, v] of Object.entries(params)) {
    q.delete(k);
    if (v === null || v === undefined || v === "" || v === false) continue;
    if (Array.isArray(v)) v.forEach((x) => q.append(k, x));
    else q.set(k, v === true ? "1" : String(v));
  }
  const s = q.toString();
  navigate(location.pathname + (s ? "?" + s : ""), opts);
}

// rangeOf turns the query into an absolute range in ms. A relative range
// is live: it moves with the clock and charts refresh.
//   r=1h|6h|24h|7d|30d|1y|all|restart, or from=&to=
export function rangeOf(q, opts = {}) {
  const now = Date.now();
  const def = opts.def || "1h";
  const from = decodeTime(q.get("from"));
  const to = decodeTime(q.get("to"));
  if (from && to && to > from) {
    return { from, to, live: false, key: "custom", label: "custom" };
  }
  if (from && !to) {
    return { from, to: now, live: true, key: "custom", label: "custom" };
  }
  let r = q.get("r") || def;
  if (r === "restart") {
    const node = opts.node;
    const start = node ? ns(node.start_time) || ns(node.up_since) : 0;
    if (start) return { from: start, to: now, live: true, key: "restart", label: "since last restart" };
    r = def;
  }
  if (r === "all") {
    const first = opts.first || now - DUR.y;
    return { from: first, to: now, live: true, key: "all", label: "all" };
  }
  const d = parseDur(r) || parseDur(def);
  return { from: now - d, to: now, live: true, key: r, label: r };
}

export function rangeParams(rg) {
  return { from: Math.floor(rg.from), to: Math.ceil(rg.to) };
}

// listParam reads a comma-separated list.
export function listParam(q, k) {
  const v = q.get(k);
  return v ? v.split(",").filter(Boolean) : [];
}

export function nodeURL(name, extra) {
  return "/node/" + encodeURIComponent(name) + (extra || "");
}
