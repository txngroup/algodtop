// Heatmap on canvas: histograms over time (dial latency, DHT RPC latency)
// and log template counts. Rows are bins or templates, columns are time
// buckets, and colour is a one-hue sequential ramp on a log scale. The SVG
// frame on top draws axes, markers and the crosshair.

import { h, s, clear } from "../dom.js";
import { Chart, defined } from "./frame.js";
import { fmtDateTime, fmtClock } from "../format.js";

const LIGHT = ["#cde2fb", "#9ec5f4", "#6da7ec", "#3987e5", "#256abf", "#184f95", "#0d366b"];
const DARK = ["#0d366b", "#184f95", "#256abf", "#3987e5", "#6da7ec", "#9ec5f4", "#cde2fb"];

function hex(c) {
  return [parseInt(c.slice(1, 3), 16), parseInt(c.slice(3, 5), 16), parseInt(c.slice(5, 7), 16)];
}

function isDark() {
  const t = document.documentElement.dataset.theme;
  if (t === "dark") return true;
  if (t === "light") return false;
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function ramp(f) {
  const stops = (isDark() ? DARK : LIGHT).map(hex);
  const p = Math.max(0, Math.min(1, f)) * (stops.length - 1);
  const i = Math.min(stops.length - 2, Math.floor(p));
  const k = p - i;
  const a = stops[i], b = stops[i + 1];
  return [0, 1, 2].map((j) => Math.round(a[j] + (b[j] - a[j]) * k));
}

// data: {from, to, step, t: [centers], rows: [{label, short, values: []}]}
// Row 0 draws at the bottom.
export class HeatmapChart extends Chart {
  constructor(opts) {
    super(Object.assign({ height: 200, toggle: false }, defined(opts)));
    this.canvas = h("canvas", { class: "heat" });
    this.canvas.style.position = "absolute";
    this.wrap.insertBefore(this.canvas, this.svg);
    this.svg.style.position = "relative";
  }

  isEmpty() {
    const d = this.data;
    return !d || !d.rows || !d.rows.length || !d.rows.some((r) => r.values.some((v) => v > 0));
  }

  yDomain(d) {
    return [0, (d.rows || []).length || 1];
  }

  yAxis(g, left, top, pw, ph) {
    const rows = (this.data && this.data.rows) || [];
    if (!rows.length) return;
    const rh = ph / rows.length;
    const every = Math.max(1, Math.ceil(12 / rh));
    rows.forEach((r, i) => {
      if (i % every) return;
      const py = top + ph - (i + 0.5) * rh;
      g.appendChild(s("text", { x: left - 6, y: py + 3.5, "text-anchor": "end" }, r.short || r.label));
    });
  }

  drawData(g, x, y, d) {
    const W = this.pw, H = this.ph;
    const dpr = window.devicePixelRatio || 1;
    const cv = this.canvas;
    cv.style.left = x(d.from) + "px";
    cv.style.top = y.range[1] + "px";
    cv.style.width = W + "px";
    cv.style.height = H + "px";
    cv.width = Math.max(1, Math.round(W * dpr));
    cv.height = Math.max(1, Math.round(H * dpr));
    const c = cv.getContext("2d");
    c.setTransform(dpr, 0, 0, dpr, 0, 0);
    c.clearRect(0, 0, W, H);
    const rows = d.rows;
    let max = 0;
    for (const r of rows) for (const v of r.values) if (v > max) max = v;
    this.max = max;
    if (!(max > 0)) return;
    const lm = Math.log1p(max);
    const rh = H / rows.length;
    const x0 = x(d.from);
    rows.forEach((r, ri) => {
      const top = H - (ri + 1) * rh;
      for (let i = 0; i < r.values.length; i++) {
        const v = r.values[i];
        if (!(v > 0)) continue;
        const a = x(d.t[i] - d.step / 2) - x0, b = x(d.t[i] + d.step / 2) - x0;
        const [cr, cg, cb] = ramp(0.08 + 0.92 * (Math.log1p(v) / lm));
        c.fillStyle = "rgb(" + cr + "," + cg + "," + cb + ")";
        c.fillRect(a, top, Math.max(1, b - a), Math.max(1, rh - (rh > 4 ? 1 : 0)));
      }
    });
  }

  render() {
    if (!this.data || this.isEmpty()) {
      const c = this.canvas.getContext("2d");
      c.clearRect(0, 0, this.canvas.width, this.canvas.height);
    }
    super.render();
  }

  // cell returns the row and column under a point.
  cellAt(t) {
    const d = this.data;
    if (!d || !d.rows || !d.t.length) return null;
    const col = Math.floor((t - (d.t[0] - d.step / 2)) / d.step);
    if (col < 0 || col >= d.t.length) return null;
    return col;
  }

  valuesAt(t) {
    const col = this.cellAt(t);
    if (col === null) return [];
    const rows = this.data.rows.map((r) => ({ r, v: r.values[col] || 0 })).filter((x) => x.v > 0);
    rows.sort((a, b) => b.v - a.v);
    return rows.slice(0, 8).map((x) => ({ id: x.r.label, name: x.r.label, color: "transparent", value: x.v, text: fmtCount(x.v) }));
  }

  valueTip(t) {
    const d = this.data;
    const col = this.cellAt(t);
    const head = col === null ? fmtDateTime(t) : fmtDateTime(d.t[col] - d.step / 2).slice(0, 11) + fmtClock(d.t[col] - d.step / 2) + " - " + fmtClock(d.t[col] + d.step / 2);
    const rows = this.valuesAt(t);
    return h("div", null, h("div", { class: "tt-time" }, head),
      rows.length ? rows.map((r) => h("div", { class: "tt-row" }, h("span", { class: "v" }, r.text), h("span", { class: "n" }, r.name))) : h("div", { class: "muted" }, "nothing in this bucket"));
  }

  renderLegend() {
    clear(this.legendEl);
    this.legendVals = null;
    if (!this.data || this.isEmpty()) return;
    const stops = (isDark() ? DARK : LIGHT).join(",");
    this.legendEl.append(
      h("span", { class: "item" }, h("span", { class: "name" }, "0")),
      h("span", { class: "item" }, h("span", { style: { display: "inline-block", width: "90px", height: "8px", borderRadius: "2px", background: "linear-gradient(90deg," + stops + ")" } })),
      h("span", { class: "item" }, h("span", { class: "name" }, fmtCount(this.max || 0) + " " + (this.o.cellUnit || "per bucket") + ", log scale")));
  }

  updateLegend() {}
}

function fmtCount(v) {
  if (v >= 1e6) return (v / 1e6).toFixed(1) + "M";
  if (v >= 1e4) return (v / 1e3).toFixed(0) + "k";
  if (v >= 100) return Math.round(v).toString();
  return (Math.round(v * 10) / 10).toString();
}
