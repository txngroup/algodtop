// Events: a filterable table of events, annotations and silences, with a
// timeline strip above it. Annotations and silences can be added here.

import { h, clear, sortableTable } from "../dom.js";
import { send, canWrite } from "../api.js";
import { rangeOf, setParams, listParam, encodeTime, decodeTime } from "../state.js";
import { fmtDateTime, fmtShortTime, toLocalInput, ns } from "../format.js";
import { rangeBar, toEv, regionsFrom, nodePicker, fetchEvents } from "../common.js";
import { Lane } from "../chart/lane.js";
import { Bus } from "../chart/crosshair.js";

const SEV_RANK = { info: 0, warn: 1, error: 2, critical: 3 };
// The lane shades down stretches whatever the kind filter, so these kinds
// are always fetched.
const REGION_KINDS = ["node-down", "node-up", "monitor-gap"];

export function mount(root, app, r) {
  let q = r.q;
  const bus = new Bus();
  const ctx = { events: [], annotations: [], regions: [], onEventClick: (ev) => setParams({ kind: ev.kind === "annotation" ? null : ev.kind, at: encodeTime(ev.t) }, { replace: true }) };
  const lane = new Lane({ bus, ctx, axis: true, height: 52 });
  const head = h("div", { class: "page-head" });
  const filters = h("div");
  const rangeEl = h("div");
  const forms = h("div");
  const tableEl = h("div");
  root.appendChild(h("div", { id: "events" }, head, rangeEl, filters, lane.el, forms, tableEl));
  let data = { events: [], counts: {}, annotations: [], silences: [] };
  let destroyed = false;
  let pending = 0;

  bus.onZoom((a, b) => setParams({ from: encodeTime(a), to: encodeTime(b), r: null }));

  function rows() {
    const kinds = new Set(listParam(q, "kind"));
    const sev = q.get("sev") || "";
    const text = (q.get("q") || "").toLowerCase();
    const out = [];
    for (const e of data.events) {
      if (e.kind === "annotation") continue;
      if (kinds.size && !kinds.has(e.kind)) continue;
      if (sev && (SEV_RANK[e.sev] || 0) < SEV_RANK[sev]) continue;
      if (text && !(e.msg + " " + e.kind + " " + (e.node || "")).toLowerCase().includes(text)) continue;
      out.push({ type: "event", t: e.t, node: e.node, kind: e.kind, sev: e.sev, msg: e.msg, ev: e });
    }
    const showNotes = !kinds.size || kinds.has("annotation");
    if (showNotes) {
      for (const a of data.annotations) {
        if (text && !a.text.toLowerCase().includes(text)) continue;
        out.push({ type: "annotation", t: a.start, end: a.end, node: a.node, kind: "annotation", sev: "info", msg: a.text + (a.silence ? " (carries a silence)" : ""), id: a.id });
      }
    }
    if (!kinds.size || kinds.has("silence")) {
      for (const sl of data.silences) {
        const msg = (sl.comment || "") + " until " + fmtDateTime(ns(sl.end)) + (sl.rules && sl.rules.length ? ", rules " + sl.rules.join(" ") : "");
        if (text && !msg.toLowerCase().includes(text)) continue;
        out.push({ type: "silence", t: ns(sl.start), end: ns(sl.end), node: (sl.nodes || []).join(" ") || "all", kind: "silence", sev: ns(sl.end) > Date.now() ? "warn" : "info", msg, id: sl.id });
      }
    }
    return out;
  }

  function drawFilters() {
    clear(filters);
    const kinds = new Set(listParam(q, "kind"));
    const all = [...new Set(Object.keys(data.counts).concat([...kinds]))].filter((k) => k !== "annotation" && k !== "silence").sort();
    all.push("annotation", "silence");
    const count = (k) => (k === "annotation" ? data.annotations.length : k === "silence" ? data.silences.length : data.counts[k] || 0);
    const toggle = (k) => {
      const next = new Set(kinds);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      setParams({ kind: [...next].join(",") || null }, { replace: true });
    };
    const text = h("input", { type: "search", placeholder: "search messages", value: q.get("q") || "", on: { change: (e) => setParams({ q: e.target.value || null }, { replace: true }) } });
    filters.append(h("div", { class: "bar" },
      h("span", { class: "muted" }, "node"),
      nodePicker(app.nodes, q.get("node") || "", (v) => setParams({ node: v || null }), { any: "all nodes" }),
      h("span", { class: "muted" }, "at least"),
      h("select", { on: { change: (e) => setParams({ sev: e.target.value || null }, { replace: true }) } },
        ["", "warn", "error", "critical"].map((v) => h("option", { value: v, selected: (q.get("sev") || "") === v }, v || "any severity"))),
      text,
      kinds.size ? h("button", { type: "button", class: "small", on: { click: () => setParams({ kind: null, at: null }, { replace: true }) } }, "clear kinds") : null),
    h("div", { class: "bar" }, all.map((k) => h("button", { type: "button", class: "small" + (kinds.has(k) ? " on" : ""), on: { click: () => toggle(k) } }, k + " " + count(k)))));
  }

  function drawForms() {
    clear(forms);
    if (!canWrite()) {
      forms.appendChild(h("p", { class: "note" }, "Log in with the full password to add annotations and silences."));
      return;
    }
    const nodeSel = () => nodePicker(app.nodes, q.get("node") || "", () => {}, { any: "all nodes" });
    // Annotation.
    const aText = h("input", { type: "text", placeholder: "text", required: true, style: { minWidth: "220px" } });
    const aNode = nodeSel();
    const aStart = h("input", { type: "datetime-local", value: toLocalInput(Date.now()) });
    const aEnd = h("input", { type: "datetime-local" });
    const aSil = h("input", { type: "text", placeholder: "silence for, e.g. 10m", style: { width: "150px" } });
    const aMsg = h("span", { class: "note" });
    const aForm = h("form", { class: "inline", on: { submit: async (e) => {
      e.preventDefault();
      try {
        const body = { text: aText.value.trim(), node: aNode.value, start: Math.round(Date.parse(aStart.value) * 1e6) };
        if (aEnd.value) body.end = Math.round(Date.parse(aEnd.value) * 1e6);
        if (aSil.value.trim()) body.silence_for = aSil.value.trim();
        await send("POST", "/api/annotations", body);
        aText.value = "";
        aMsg.textContent = "Saved.";
        load();
      } catch (err) {
        aMsg.textContent = err.message;
      }
    } } }, aText, aNode, "from", aStart, "to", aEnd, aSil, h("button", { type: "submit", class: "primary" }, "add annotation"), aMsg);
    // Silence.
    const sNodes = h("select", { multiple: true, size: Math.min(10, Math.max(2, app.nodes.length)), title: "No selection means every node" }, app.nodes.map((n) => h("option", { value: n.name }, n.name)));
    const sRules = h("input", { type: "text", placeholder: "rules, comma separated (empty: all)", style: { width: "220px" } });
    const sFor = h("input", { type: "text", value: "10m", style: { width: "70px" }, "aria-label": "Duration" });
    const sComment = h("input", { type: "text", placeholder: "comment", required: true, style: { minWidth: "200px" } });
    const sMsg = h("span", { class: "note" });
    const sForm = h("form", { class: "inline", on: { submit: async (e) => {
      e.preventDefault();
      try {
        const nodes = [...sNodes.selectedOptions].map((o) => o.value);
        const rules = sRules.value.split(",").map((x) => x.trim()).filter(Boolean);
        await send("POST", "/api/silences", { nodes, rules, for: sFor.value.trim(), comment: sComment.value.trim() });
        sComment.value = "";
        sMsg.textContent = "Silence added.";
        load();
      } catch (err) {
        sMsg.textContent = err.message;
      }
    } } }, sNodes, sRules, "for", sFor, sComment, h("button", { type: "submit", class: "primary" }, "add silence"), sMsg);
    forms.append(h("fieldset", null, h("legend", null, "Add annotation"), aForm),
      h("fieldset", null, h("legend", null, "Add silence"), h("p", { class: "note", style: { margin: "0 0 6px" } }, "A silence stops notifications. Health and events are still recorded."), sForm));
  }

  function drawTable() {
    clear(tableEl);
    const list = rows();
    const at = decodeTime(q.get("at"));
    let hl = null;
    if (at) {
      let bd = Infinity;
      for (const r2 of list) {
        const d = Math.abs(r2.t - at);
        if (d < bd) {
          bd = d;
          hl = r2;
        }
      }
    }
    const del = async (row) => {
      if (!confirm("Delete this " + row.type + "?")) return;
      try {
        await send("DELETE", row.type === "annotation" ? "/api/annotations" : "/api/silences", undefined, { id: row.id });
        load();
      } catch (e) {
        alert(e.message);
      }
    };
    const cols = [
      { key: "t", label: "time", num: true, render: (x) => fmtDateTime(x.t) + (x.end ? " to " + fmtShortTime(x.end) : "") },
      { key: "node", label: "node", cls: "nowrap", render: (x) => x.node || "all" },
      { key: "type", label: "type" },
      { key: "kind", label: "kind", render: (x) => h("span", { class: "sev " + x.sev }, x.kind) },
      { key: "sev", label: "severity", get: (x) => SEV_RANK[x.sev] || 0, render: (x) => x.sev },
      { key: "msg", label: "message", cls: "wrap" },
    ];
    if (canWrite()) cols.push({ key: "act", label: "", sort: false, render: (x) => (x.type === "event" ? "" : h("button", { type: "button", class: "small danger", on: { click: (e) => { e.stopPropagation(); del(x); } } }, "delete")) });
    const tbl = sortableTable(cols, list, { sort: "t", desc: true, limit: 2000, empty: "No events match.", rowClass: (x) => (x === hl ? "hl" : "") });
    // The server keeps the newest 5000 events of the kinds asked; say how
    // many older ones that left out.
    const kinds = listParam(q, "kind");
    const want = (k) => k !== "annotation" && (!kinds.length || kinds.includes(k));
    const held = Object.keys(data.counts).filter(want).reduce((a, k) => a + data.counts[k], 0);
    const have = data.events.filter((e) => want(e.kind)).length;
    tableEl.append(h("p", { class: "note" }, list.length + " rows" + (list.length > 2000 ? ", showing the newest 2000" : "") + (have < held ? ". " + (held - have) + " older events are left out; narrow the range or filter by kind." : "")),
      h("div", { class: "table-wrap" }, tbl));
    if (hl) {
      const tr = tbl.querySelector("tr.hl");
      if (tr) setTimeout(() => tr.scrollIntoView({ block: "center" }), 0);
    }
  }

  function drawLane() {
    const rg = rangeOf(q, { def: "24h" });
    const visible = rows();
    ctx.events = visible.filter((x) => x.type === "event").map((x) => x.ev);
    ctx.annotations = data.annotations;
    ctx.regions = regionsFrom(data.events, q.get("node") || "", Date.now());
    lane.setRange(rg.from, rg.to);
  }

  async function load() {
    const rg = rangeOf(q, { def: "24h" });
    clear(head);
    head.append(h("h1", null, "Events"));
    clear(rangeEl);
    rangeEl.appendChild(rangeBar(q, { def: "24h", raw: false, restart: false }));
    try {
      const kinds = listParam(q, "kind");
      const ask = kinds.length ? [...new Set(kinds.concat(REGION_KINDS))] : null;
      const res = await fetchEvents(q.get("node") || "", rg.from, rg.to, ask);
      if (destroyed) return;
      data = { events: res.events, counts: res.counts, annotations: res.annotations, silences: res.silences };
    } catch (e) {
      clear(tableEl);
      tableEl.appendChild(h("p", { class: "error" }, e.message));
      return;
    }
    drawFilters();
    drawLane();
    drawForms();
    drawTable();
  }

  load();
  const timer = setInterval(() => rangeOf(q, { def: "24h" }).live && load(), 60e3);

  return {
    update(r2) {
      const prev = q;
      q = r2.q;
      if (["r", "from", "to", "node", "kind"].some((k) => prev.get(k) !== q.get(k))) load();
      else {
        drawFilters();
        drawLane();
        drawTable();
      }
    },
    event(e) {
      const node = q.get("node");
      if (node && e.node !== node) return;
      const rg = rangeOf(q, { def: "24h" });
      if (!rg.live) return;
      data.counts[e.kind] = (data.counts[e.kind] || 0) + 1;
      const kinds = listParam(q, "kind");
      if (kinds.length && !kinds.includes(e.kind) && !REGION_KINDS.includes(e.kind)) return;
      data.events.unshift(toEv(e));
      // Busy nodes send many events; redraw at most every 2 s.
      if (pending) return;
      pending = setTimeout(() => {
        pending = 0;
        if (destroyed) return;
        drawLane();
        drawTable();
      }, 2000);
    },
    destroy() {
      destroyed = true;
      clearInterval(timer);
      clearTimeout(pending);
      lane.destroy();
    },
  };
}
