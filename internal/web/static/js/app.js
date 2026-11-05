// Boot, routing, theme and the status line. Every page is a module with
// mount(root, app) that returns {update, status, event, destroy}.

import { h, clear } from "./dom.js";
import { get, loadSession, session, setAuthLostHandler } from "./api.js";
import { route, setRouteHandler, navigate, nodeURL } from "./state.js";
import * as live from "./live.js";
import { fmt, fmtDur, ns } from "./format.js";
import { healthLevel } from "./common.js";
import * as fleet from "./pages/fleet.js";
import * as nodePage from "./pages/node.js";
import * as peers from "./pages/peers.js";
import * as events from "./pages/events.js";
import * as compare from "./pages/compare.js";
import * as explorer from "./pages/explorer.js";
import * as settings from "./pages/settings.js";
import * as login from "./pages/login.js";

const PAGES = { fleet, node: nodePage, peers, events, compare, explorer, settings };

export const app = {
  nodes: [],
  status: null,
  lastNode: "",
  byName(name) {
    return this.nodes.find((n) => n.name === name);
  },
};

let current = null;
let currentKey = "";

// Theme: auto follows the OS; light and dark are stored choices.
const THEMES = ["auto", "light", "dark"];
function getTheme() {
  try {
    return localStorage.getItem("algodtop.theme") || "auto";
  } catch (e) {
    return "auto";
  }
}
function setTheme(t) {
  try {
    if (t === "auto") localStorage.removeItem("algodtop.theme");
    else localStorage.setItem("algodtop.theme", t);
  } catch (e) {
    /* storage may be blocked; the choice lasts for this page only */
  }
  applyTheme(t);
}
function applyTheme(t) {
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
  const b = document.getElementById("theme");
  if (b) b.textContent = "theme: " + t;
  if (current && current.theme) current.theme();
}

function renderStatusLine() {
  const el = document.getElementById("statusline");
  if (!el) return;
  const st = app.status;
  const counts = { ok: 0, warn: 0, critical: 0, down: 0 };
  for (const n of app.nodes) counts[healthLevel(n)]++;
  const parts = [];
  if (st) {
    parts.push("host " + st.host);
    if (st.started) parts.push("daemon up " + fmtDur((Date.now() - ns(st.started)) / 1000));
    if (st.store_bytes) parts.push("store " + fmt(st.store_bytes, "bytes"));
    if (st.live) parts.push("no daemon: live only, no history");
    if (st.paused) parts.push("writes paused");
  }
  const sum = [];
  if (counts.ok) sum.push(counts.ok + " OK");
  if (counts.warn) sum.push(counts.warn + " Warn");
  if (counts.critical) sum.push(counts.critical + " Critical");
  if (counts.down) sum.push(counts.down + " Down");
  parts.push("nodes " + app.nodes.length + (sum.length ? ": " + sum.join(", ") : ""));
  if (st && st.warnings && st.warnings.length) parts.push(st.warnings.join("; "));
  el.textContent = parts.join("   ");
}

function setNav(page) {
  for (const a of document.querySelectorAll("#nav a")) {
    a.classList.toggle("active", a.dataset.page === page);
    if (a.dataset.page === "node") a.href = app.lastNode ? nodeURL(app.lastNode) : "/node";
    if (a.dataset.page === "peers") a.href = app.lastNode ? "/peers/" + encodeURIComponent(app.lastNode) : "/peers";
  }
}

function render() {
  const r = route();
  const root = document.getElementById("app");
  if (session.auth_required && session.role === "none") {
    if (current && current.destroy) current.destroy();
    current = login.mount(root, app, () => boot());
    currentKey = "login";
    return;
  }
  let page = PAGES[r.page] ? r.page : "fleet";
  if ((page === "node" || page === "peers") && !r.node) {
    const first = app.lastNode || (app.nodes[0] && app.nodes[0].name);
    if (first) {
      navigate("/" + page + "/" + encodeURIComponent(first) + location.search, { replace: true });
      return;
    }
  }
  if (r.node) app.lastNode = r.node;
  setNav(page);
  const key = page + "/" + r.node;
  if (current && key === currentKey && current.update) {
    current.update(r);
    return;
  }
  if (current && current.destroy) current.destroy();
  clear(root);
  currentKey = key;
  try {
    current = PAGES[page].mount(root, app, r) || {};
  } catch (e) {
    console.error(e);
    root.appendChild(h("p", { class: "error" }, "This page failed to load: " + e.message));
    current = {};
  }
}

async function boot() {
  try {
    await loadSession();
  } catch (e) {
    document.getElementById("app").textContent = "Cannot reach the algodtop API: " + e.message;
    return;
  }
  if (session.auth_required && session.role === "none") {
    render();
    return;
  }
  try {
    const r = await get("/api/nodes");
    app.nodes = r.nodes || [];
    app.status = r.status || null;
  } catch (e) {
    if (session.auth_required && session.role === "none") return;
    app.nodes = [];
  }
  renderStatusLine();
  live.start(app.nodes.map((n) => n.name));
  render();
}

live.on("status", (d) => {
  // The stream sends every node, so it replaces the list.
  if (d.nodes && d.nodes.length) app.nodes = d.nodes;
  if (d.status) app.status = d.status;
  renderStatusLine();
  if (current && current.status) current.status(d);
});
live.on("event", (e) => {
  if (current && current.event) current.event(e);
});
live.on("conn", (ok) => {
  const el = document.getElementById("conn");
  if (!el) return;
  el.textContent = ok ? "live" : "reconnecting";
  el.className = "conn " + (ok ? "live" : "down");
});

setAuthLostHandler(() => {
  if (session.auth_required) {
    session.role = "none";
    live.stop();
    render();
  }
});

setRouteHandler(render);
window.addEventListener("popstate", render);

// Internal links navigate without a page load.
document.addEventListener("click", (e) => {
  const a = e.target.closest && e.target.closest("a");
  if (!a || a.target || a.hasAttribute("download") || e.ctrlKey || e.metaKey || e.shiftKey || e.button !== 0) return;
  const href = a.getAttribute("href");
  if (!href || !href.startsWith("/") || href.startsWith("/api/") || href.startsWith("//")) return;
  e.preventDefault();
  navigate(href);
});

document.addEventListener("DOMContentLoaded", () => {
  applyTheme(getTheme());
  document.getElementById("theme").addEventListener("click", () => {
    const t = getTheme();
    setTheme(THEMES[(THEMES.indexOf(t) + 1) % THEMES.length]);
  });
  boot();
});
if (window.matchMedia) {
  const mq = window.matchMedia("(prefers-color-scheme: dark)");
  if (mq.addEventListener) mq.addEventListener("change", () => current && current.theme && current.theme());
}
