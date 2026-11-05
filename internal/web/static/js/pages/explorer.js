// Explorer: pick any stored metric and a label filter and get a chart. It
// covers questions nobody anticipated, without code changes.

import { h, clear } from "../dom.js";
import { get, qs } from "../api.js";
import { rangeOf, setParams, encodeTime, navigate } from "../state.js";
import { rangeBar, nodePicker, loadMarkers } from "../common.js";
import { Bus } from "../chart/crosshair.js";
import { Panel } from "../panel.js";
import { fmt } from "../format.js";
import { isVal } from "../chart/frame.js";

const FNS = ["", "rate", "increase", "avg", "min", "max", "last"];
const HIST_FNS = ["p50", "p90", "p99", "p999", "heatmap"];

function guessUnit(name) {
  if (/_bytes_total$/.test(name)) return "B/s";
  if (/_bytes$/.test(name) || /_bytes_bucket$/.test(name)) return "bytes";
  if (/_seconds_total$/.test(name)) return "";
  if (/_seconds(_bucket)?$/.test(name)) return "s";
  if (/_milliseconds_bucket$/.test(name)) return "";
  return "";
}

export function mount(root, app, r) {
  let q = r.q;
  const bus = new Bus();
  const ctx = { events: [], annotations: [], regions: [],
    onEventClick: (ev) => navigate("/events?node=" + encodeURIComponent(ev.node || "") + (ev.kind === "annotation" ? "" : "&kind=" + encodeURIComponent(ev.kind)) + "&at=" + encodeTime(ev.t)) };
  const head = h("div", { class: "page-head" }, h("h1", null, "Explorer"));
  const ctl = h("div");
  const rangeEl = h("div");
  const chartEl = h("div", { class: "grid one" });
  const listEl = h("div");
  root.appendChild(h("div", { id: "explorer" }, head, ctl, rangeEl, chartEl, listEl));
  let metrics = [];
  let metricsNode = "";
  let labels = {};
  let panel = null;
  let destroyed = false;

  const node = () => q.get("node") || app.lastNode || (app.nodes[0] && app.nodes[0].name) || "";

  async function loadMetrics() {
    const n = node();
    if (metricsNode === n) return;
    metricsNode = n;
    try {
      const res = await get("/api/metrics", { node: n });
      metrics = res.metrics || [];
    } catch (e) {
      metrics = [];
    }
  }

  async function loadLabels(metric) {
    labels = {};
    if (!metric) return;
    try {
      const res = await get("/api/metrics", { node: node(), q: metric, labels: 1 });
      const m = (res.metrics || []).find((x) => x.name === metric);
      labels = (m && m.labels) || {};
    } catch (e) {
      labels = {};
    }
  }

  function drawControls() {
    const metric = q.get("metric") || "";
    const meta = metrics.find((m) => m.name === metric);
    const isHist = /_bucket$/.test(metric) && meta && meta.kind === "histogram";
    const matches = q.getAll("match");
    clear(ctl);
    const dl = h("datalist", { id: "metric-names" }, metrics.map((m) => h("option", { value: m.name }, m.kind + ", " + m.series + " series")));
    const mi = h("input", { type: "text", list: "metric-names", value: metric, placeholder: "metric name, e.g. algodtop_peers", style: { minWidth: "320px" },
      on: { change: (e) => setParams({ metric: e.target.value.trim() || null, match: null, by: null, fn: null }) } });
    const fnSel = h("select", { "aria-label": "Function", on: { change: (e) => setParams({ fn: e.target.value || null }) } },
      FNS.concat(isHist ? HIST_FNS : []).map((f) => h("option", { value: f, selected: (q.get("fn") || "") === f }, f || "default fn")));
    const bySel = h("select", { "aria-label": "Sum by label", on: { change: (e) => setParams({ by: e.target.value || null }) } },
      h("option", { value: "" }, "no grouping"), Object.keys(labels).filter((k) => k !== "le").map((k) => h("option", { value: k, selected: q.get("by") === k }, "sum by " + k)));
    const typeSel = h("select", { "aria-label": "Chart type", on: { change: (e) => setParams({ type: e.target.value === "line" ? null : e.target.value }) } },
      ["line", "step", "area", "bars"].map((t) => h("option", { value: t, selected: (q.get("type") || "line") === t }, t)));
    const filters = h("div", { class: "bar" });
    for (const [k, vals] of Object.entries(labels)) {
      if (k === "le") continue;
      const cur = matches.find((m) => m.startsWith(k + ":"));
      const sel = h("select", { "aria-label": k, on: { change: (e) => {
        const rest = matches.filter((m) => !m.startsWith(k + ":"));
        if (e.target.value) rest.push(k + ":" + e.target.value);
        setParams({ match: rest.length ? rest : null });
      } } }, h("option", { value: "" }, k + ": any"), vals.slice().sort().map((v) => h("option", { value: v, selected: cur === k + ":" + v }, k + " = " + (v.length > 60 ? v.slice(0, 60) + "..." : v))));
      filters.appendChild(sel);
    }
    const free = h("input", { type: "text", placeholder: "label:value, label:!value or label:prefix*", style: { minWidth: "240px" } });
    const addFree = h("button", { type: "button", on: { click: () => {
      if (!free.value.includes(":")) return;
      setParams({ match: matches.concat([free.value.trim()]) });
    } } }, "add filter");
    ctl.append(dl, h("div", { class: "bar" },
      h("span", { class: "muted" }, "node"), nodePicker(app.nodes, node(), (v) => setParams({ node: v })),
      mi, fnSel, bySel, typeSel),
    filters.childNodes.length ? filters : h("span"),
    h("div", { class: "bar" }, free, addFree, matches.map((m) => h("span", { class: "chip" }, m, h("button", { type: "button", title: "Remove", on: { click: () => setParams({ match: matches.filter((x) => x !== m) }) } }, "x")))));
    clear(rangeEl);
    rangeEl.appendChild(rangeBar(q, { node: app.byName(node()) }));
  }

  function drawList() {
    clear(listEl);
    if (q.get("metric")) return;
    const filter = (q.get("f") || "").toLowerCase();
    const shown = metrics.filter((m) => !filter || m.name.toLowerCase().includes(filter));
    listEl.append(h("div", { class: "bar" }, h("input", { type: "search", placeholder: "filter metric names", value: q.get("f") || "", on: { change: (e) => setParams({ f: e.target.value || null }, { replace: true }) } }),
      h("span", { class: "note" }, shown.length + " of " + metrics.length + " metrics stored for " + node())),
    h("div", { class: "table-wrap", style: { maxHeight: "600px", overflowY: "auto" } }, h("table", { class: "t" },
      h("thead", null, h("tr", null, h("th", null, "metric"), h("th", null, "kind"), h("th", { class: "num" }, "series"))),
      h("tbody", null, shown.slice(0, 1000).map((m) => h("tr", { class: "click", on: { click: () => setParams({ metric: m.name, f: null }) } },
        h("td", { class: "mono" }, m.name), h("td", null, m.kind), h("td", { class: "num" }, String(m.series))))))));
  }

  function drawChart() {
    if (panel) panel.destroy();
    panel = null;
    clear(chartEl);
    const metric = q.get("metric");
    if (!metric) return;
    const fn = q.get("fn") || "";
    const type = fn === "heatmap" ? "heatmap" : q.get("type") || "line";
    const meta = metrics.find((m) => m.name === metric);
    let unit = guessUnit(metric);
    if (meta && meta.kind === "counter" && (fn === "" || fn === "rate") && !unit.endsWith("/s")) unit = unit === "" ? "/s" : unit;
    if (fn === "increase") unit = unit === "B/s" ? "bytes" : "";
    const spec = {
      id: "explore", title: metric, unit, type, height: 300, band: type === "line",
      sub: [fn || "default fn", q.get("by") ? "sum by " + q.get("by") : "", q.getAll("match").join(" ")].filter(Boolean).join(", "),
      q: [{ metric, fn: fn || undefined, by: q.get("by") || undefined, match: q.getAll("match"), limit: 50 }],
    };
    if (type === "heatmap") {
      spec.load = async (view, pts, p) => {
        const cols = Math.max(20, Math.min(240, Math.floor(pts / 5)));
        const ep = { node: view.node, metric, fn: "heatmap", match: q.getAll("match"), from: Math.floor(view.from), to: Math.ceil(view.to), points: cols };
        const res = await get("/api/series", ep);
        p.chart.setExports([{ label: "JSON", href: "/api/export" + qs(Object.assign({ format: "json" }, ep)) }]);
        const hm = res.heatmaps || [];
        const step = res.step / 1e6;
        if (!hm.length) return { from: view.from, to: view.to, step, t: [], rows: [], parts: [] };
        const le = hm[0].le;
        const rows = le.map((v, i) => ({ label: v === null ? "> " + fmt(le[i - 1], "") : "<= " + fmt(v, ""), short: v === null ? "inf" : fmt(v, ""), values: new Array(cols).fill(0) }));
        for (const x of hm) x.counts.forEach((c, b) => c.forEach((v, j) => { if (isVal(v) && rows[j]) rows[j].values[b] += v; }));
        return { from: res.from / 1e6, to: res.to / 1e6, step, t: hm[0].t.map((x) => x / 1e6 + step / 2), rows, parts: (res.parts || []).map((p) => ({ from: p.from / 1e6, to: p.to / 1e6, tier: p.tier })) };
      };
      spec.cellUnit = "observations";
    }
    panel = new Panel(spec, { bus, ctx, relative: false });
    chartEl.appendChild(panel.el);
    const rg = rangeOf(q, { def: "1h", node: app.byName(node()) });
    const view = { node: node(), from: rg.from, to: rg.to, live: rg.live, raw: q.get("raw") === "1", key: "x" };
    loadMarkers(ctx, node(), rg.from, rg.to).then(() => panel && panel.chart.data && panel.chart.render()).catch(() => {});
    panel.load(view);
  }

  async function render(full) {
    await loadMetrics();
    if (full !== false) await loadLabels(q.get("metric"));
    if (destroyed) return;
    drawControls();
    drawList();
    drawChart();
  }

  bus.onZoom((a, b) => setParams({ from: encodeTime(a), to: encodeTime(b), r: null }));
  render();
  const timer = setInterval(() => {
    if (!panel || !rangeOf(q, {}).live) return;
    const rg = rangeOf(q, { def: "1h" });
    panel.load({ node: node(), from: rg.from, to: rg.to, live: true, raw: q.get("raw") === "1", key: "x" }, { tail: true });
  }, 15e3);

  return {
    update(r2) {
      const prev = q;
      q = r2.q;
      const metricChanged = prev.get("metric") !== q.get("metric") || prev.get("node") !== q.get("node");
      render(metricChanged);
    },
    destroy() {
      destroyed = true;
      clearInterval(timer);
      if (panel) panel.destroy();
    },
  };
}
