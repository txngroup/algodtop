// Settings: nodes, retention and auth, read-only by default. Nodes and
// retention can be edited only when the daemon says so and the session
// can write. Alert channels are config-file only and never editable here.

import { h, clear } from "../dom.js";
import { get, send, canWrite, session, logout } from "../api.js";
import { fmt, fmtDateTime, fmtDur, ns } from "../format.js";

// byTier lists a map keyed by tier from finest to coarsest. JSON objects
// from Go arrive sorted by key, which put 10m before 1h before 1m.
const TIERS = ["raw", "1m", "10m", "1h"];
function byTier(m) {
  const rank = (k) => (TIERS.indexOf(k) < 0 ? TIERS.length : TIERS.indexOf(k));
  return Object.entries(m || {}).sort((a, b) => rank(a[0]) - rank(b[0]) || (a[0] < b[0] ? -1 : 1));
}

function kvTable(obj) {
  const rows = Object.entries(obj || {}).filter(([, v]) => v !== undefined && v !== null && v !== "" && !(Array.isArray(v) && !v.length));
  if (!rows.length) return h("p", { class: "muted" }, "Defaults.");
  return h("div", { class: "table-wrap" }, h("table", { class: "t kvtable" }, h("tbody", null, rows.map(([k, v]) =>
    h("tr", null, h("td", null, k), h("td", { class: "wrap mono" }, typeof v === "object" ? JSON.stringify(v) : String(v)))))));
}

export function mount(root, app) {
  const el = h("div", { id: "settings" }, h("h1", null, "Settings"), h("p", { class: "muted" }, "Loading..."));
  root.appendChild(el);
  let destroyed = false;

  async function load() {
    let cfg, st, disc;
    try {
      [cfg, st, disc] = await Promise.all([get("/api/config"), get("/api/status"), get("/api/discovered").catch(() => ({ discovered: [] }))]);
    } catch (e) {
      clear(el);
      el.append(h("h1", null, "Settings"), h("p", { class: "error" }, e.message));
      return;
    }
    if (destroyed) return;
    const c = cfg.config || {};
    const editable = !!cfg.editable && canWrite();
    clear(el);
    el.append(h("div", { class: "page-head" }, h("h1", null, "Settings"),
      h("span", { class: "note" }, cfg.path ? "config file " + cfg.path : ""),
      h("span", { class: "note" }, cfg.editable ? (canWrite() ? "Nodes and retention can be edited here." : "Log in with the full password to edit.") : "Read-only. Set web.allow_config_edits in the config file to edit nodes and retention here.")));

    // Daemon and session.
    el.append(h("h2", null, "Daemon"), h("dl", { class: "kvl", style: { margin: "8px 0 16px" } },
      h("dt", null, "host"), h("dd", null, st.host || "-"),
      h("dt", null, "version"), h("dd", null, (st.version || "-") + (st.version_time ? ", committed " + fmtDateTime(ns(st.version_time)) + " (" + fmtDur((Date.now() - ns(st.version_time)) / 1000) + " ago)" : "")),
      st.latest ? h("dt", null, "newer release") : null,
      st.latest ? h("dd", null, "committed " + fmtDateTime(ns(st.latest_time)) + ". Run ", h("code", null, "algod.top upgrade"), " on this machine to install it here and on every remote agent.") : null,
      h("dt", null, "platform"), h("dd", null, st.platform || "-"),
      h("dt", null, "started"), h("dd", null, st.started ? fmtDateTime(ns(st.started)) + " (" + fmtDur((Date.now() - ns(st.started)) / 1000) + " ago)" : "-"),
      h("dt", null, "store"), h("dd", null, fmt(st.store_bytes || 0, "bytes") + "  " + byTier(st.tier_bytes).map(([k, v]) => k + " " + fmt(v, "bytes")).join(", ")),
      h("dt", null, "retention"), h("dd", null, byTier(st.retention).map(([k, v]) => k + " " + v).join(", ")),
      h("dt", null, "algod.top itself"), h("dd", null, st.self ? "CPU " + (st.self.cpu || 0).toFixed(1) + "%, RSS " + fmt(st.self.rss || 0, "bytes") + ", " + st.self.sse_clients + " live viewers, " + (st.self.samples || 0).toLocaleString("en-US") + " samples stored" : "-"),
      h("dt", null, "session"), h("dd", null, session.auth_required ? "logged in as " + session.role + " " : "no password set; this listener allows writes from this machine ",
        session.auth_required ? h("button", { type: "button", class: "small", on: { click: async () => { await logout(); location.reload(); } } }, "log out") : null)));

    // Nodes.
    el.appendChild(h("h2", null, "Nodes"));
    const nodes = (c.nodes || []).map((n) => Object.assign({}, n));
    const nodesSlot = h("div");
    el.appendChild(nodesSlot);
    const drawNodes = () => {
      clear(nodesSlot);
      const cols = ["name", "datadir", "url", "token", "admin_token", "accounts", "disabled"];
      const tbl = h("table", { class: "t" }, h("thead", null, h("tr", null, cols.map((k) => h("th", null, k)), editable ? h("th") : null)),
        h("tbody", null, nodes.map((n, i) => h("tr", null, cols.map((k) => {
          if (!editable || k === "token" || k === "admin_token") {
            const v = n[k];
            return h("td", { class: "wrap mono" }, Array.isArray(v) ? v.join(", ") : v === true ? "yes" : v === undefined || v === false ? "" : String(v));
          }
          if (k === "disabled") return h("td", null, h("input", { type: "checkbox", checked: !!n.disabled, on: { change: (e) => (n.disabled = e.target.checked || undefined) } }));
          const val = k === "accounts" ? (n.accounts || []).join(", ") : n[k] || "";
          return h("td", null, h("input", { type: "text", value: val, style: { width: "100%" }, on: { change: (e) => {
            const v = e.target.value.trim();
            if (k === "accounts") n.accounts = v ? v.split(",").map((x) => x.trim()).filter(Boolean) : undefined;
            else n[k] = v || undefined;
          } } }));
        }), editable ? h("td", null, h("button", { type: "button", class: "small danger", on: { click: () => { nodes.splice(i, 1); drawNodes(); } } }, "remove")) : null))));
      nodesSlot.append(h("div", { class: "table-wrap" }, tbl));
      if (editable) nodesSlot.appendChild(h("div", { class: "bar", style: { marginTop: "6px" } }, h("button", { type: "button", on: { click: () => { nodes.push({ name: "" }); drawNodes(); } } }, "add node")));
    };
    drawNodes();

    // Retention.
    el.appendChild(h("h2", { style: { marginTop: "16px" } }, "Retention"));
    const ret = Object.assign({}, c.retention || {});
    if (editable) {
      const fields = [["raw", "raw samples (default 30d)"], ["1m", "1m buckets (default 1y)"], ["10m", "10m buckets (default 5y)"], ["1h", "1h buckets (default forever)"], ["peer_addresses", "peer addresses (default 1y)"]];
      el.appendChild(h("div", { class: "bar" }, fields.map(([k, label]) => h("label", null, label + " ", h("input", { type: "text", value: ret[k] || "", placeholder: "default", style: { width: "90px" }, on: { change: (e) => {
        if (e.target.value.trim()) ret[k] = e.target.value.trim();
        else delete ret[k];
      } } })))));
      const msg = h("span", { class: "note" });
      el.appendChild(h("div", { class: "bar" }, h("button", { type: "button", class: "primary", on: { click: async () => {
        msg.textContent = "Saving...";
        try {
          const clean = nodes.filter((n) => n.name && n.name.trim());
          await send("PUT", "/api/config", { nodes: clean, retention: ret });
          msg.textContent = "Saved. The config file was written and reloaded.";
        } catch (e) {
          msg.textContent = e.message;
          msg.className = "error";
        }
      } } }, "save nodes and retention"), msg));
    } else {
      el.appendChild(kvTable(ret));
    }

    // Read-only sections.
    const web = Object.assign({}, c.web || {});
    for (const k of ["password_hash", "read_only_password_hash"]) if (web[k]) web[k] = "(set)";
    el.append(h("h2", { style: { marginTop: "16px" } }, "Web listener and auth"), kvTable(web),
      h("h2", { style: { marginTop: "16px" } }, "Disk floor"), kvTable(c.disk),
      h("h2", { style: { marginTop: "16px" } }, "Collection intervals"), kvTable(c.intervals),
      h("h2", { style: { marginTop: "16px" } }, "Series caps"), kvTable(c.caps),
      h("h2", { style: { marginTop: "16px" } }, "Logs"), kvTable(c.logs));
    el.appendChild(h("h2", { style: { marginTop: "16px" } }, "Alert rules"));
    const rules = c.alerts || [];
    el.appendChild(rules.length ? h("div", { class: "table-wrap" }, h("table", { class: "t" },
      h("thead", null, h("tr", null, ["name", "expression", "for", "severity", "repeat", "channels"].map((k) => h("th", null, k)))),
      h("tbody", null, rules.map((a) => h("tr", null, h("td", null, a.name || ""), h("td", { class: "mono" }, a.expr || a.expression || ""), h("td", null, a.for || ""), h("td", null, a.severity || ""), h("td", null, a.repeat || ""), h("td", null, (a.channels || []).join(", "))))))) : h("p", { class: "muted" }, "Only the built-in health rules."));
    el.appendChild(h("h2", { style: { marginTop: "16px" } }, "Alert channels"));
    const ch = Object.entries(c.channels || {});
    el.append(h("p", { class: "note" }, "Channels are defined only in the config file. A command channel runs a program as the daemon's user, so neither the web UI nor the API can create or change one."),
      ch.length ? h("ul", { class: "list-plain" }, ch.map(([k, v]) => h("li", null, k + " (" + (v.type || "?") + ")"))) : h("p", { class: "muted" }, "No channels configured."));
    el.appendChild(h("h2", { style: { marginTop: "16px" } }, "Discovered nodes not in the config"));
    const ds = disc.discovered || [];
    el.appendChild(ds.length ? h("div", { class: "table-wrap" }, h("table", { class: "t" },
      h("thead", null, h("tr", null, ["name", "data dir", "network", "running"].map((k) => h("th", null, k)))),
      h("tbody", null, ds.map((d) => h("tr", null, h("td", null, d.name), h("td", { class: "mono" }, d.datadir), h("td", null, d.network || ""), h("td", null, d.running ? "yes" : "no")))))) : h("p", { class: "muted" }, "None."));
    if (ds.length) el.appendChild(h("p", { class: "note" }, "Run algod.top daemon --discover to add them, or add them to the config file."));
  }

  load();
  return { destroy() { destroyed = true; } };
}
