// Chart is the frame every chart type shares: title, units, legend, axes,
// event and annotation markers, hatched down-time regions, tier labels,
// the synchronized crosshair, drag-to-zoom and export links. Chart types
// (line.js, area.js, bars.js, heatmap.js) supply yDomain, drawData and
// valuesAt.

import { h, s, clear, showTip, hideTip } from "../dom.js";
import { linear, niceTicks, timeTicks, offsetTicks, bisect } from "./scale.js";
import { fmt, axisFormat, unitInfo, fmtDateTime, fmtClock, isNum } from "../format.js";

export const ML = 58, MR = 12, MT = 6, MB = 20;
const TIER_ORDER = { raw: 0, "1m": 1, "10m": 2, "1h": 3 };

// insertGaps puts a null between samples further apart than factor times
// the median spacing, so raw data with holes draws as separate runs.
export function insertGaps(t, v, factor = 3, minGap = 0) {
  if (t.length < 3) return { t, v };
  const d = [];
  for (let i = 1; i < t.length; i++) d.push(t[i] - t[i - 1]);
  const sorted = d.slice().sort((a, b) => a - b);
  const med = sorted[sorted.length >> 1] || 0;
  const lim = Math.max(med * factor, minGap);
  if (!(lim > 0)) return { t, v };
  const ot = [t[0]], ov = [v[0]];
  for (let i = 1; i < t.length; i++) {
    if (t[i] - t[i - 1] > lim) {
      ot.push((t[i] + t[i - 1]) / 2);
      ov.push(null);
    }
    ot.push(t[i]);
    ov.push(v[i]);
  }
  return { t: ot, v: ov, med };
}

export const isVal = (v) => v !== null && v !== undefined && !Number.isNaN(v);

// defined drops undefined options so they do not hide the defaults.
export function defined(o) {
  const out = {};
  for (const [k, v] of Object.entries(o || {})) if (v !== undefined) out[k] = v;
  return out;
}

export class Chart {
  constructor(opts) {
    this.o = Object.assign({ height: 170, zero: true, unit: "" }, defined(opts));
    this.bus = this.o.bus || null;
    this.ctx = this.o.ctx || {};
    this.data = null;
    this.width = 0;
    this.hiddenSeries = new Set();
    this.ready = new Promise((res) => (this._ready = res));

    this.titleEl = h("span", { class: "chart-title" }, this.o.title || "");
    this.unitEl = h("span", { class: "chart-unit" }, this.o.unitLabel !== undefined ? this.o.unitLabel : unitLabel(this.o.unit));
    this.subEl = h("div", { class: "chart-sub" }, this.o.sub || "");
    this.actions = h("div", { class: "chart-actions" });
    this.legendEl = h("div", { class: "legend" });
    this.svg = s("svg", { class: "plot", height: this.o.height });
    this.wrap = h("div", { class: "heat-wrap" }, this.svg);
    this.footEl = h("div", { class: "chart-foot" });
    this.el = h("div", { class: "chart" },
      h("div", { class: "chart-head" }, h("div", null, h("div", null, this.titleEl, this.unitEl), this.subEl), this.actions),
      this.legendEl, this.wrap, this.footEl);

    this.ro = new ResizeObserver((entries) => {
      const w = Math.floor(entries[0].contentRect.width);
      if (w > 0 && w !== this.width) {
        this.width = w;
        this._ready(w);
        this.render();
      }
    });
    this.ro.observe(this.wrap);
    this.unsub = this.bus ? this.bus.on((t, src) => this.cross(t, src)) : null;
  }

  destroy() {
    this.ro.disconnect();
    if (this.unsub) this.unsub();
  }

  // plotWidth is the number of pixels the data spans, which is also the
  // number of points to ask the API for.
  plotWidth() {
    const w = this.width || (this.el.parentElement ? this.el.parentElement.clientWidth : 600) || 600;
    return Math.max(20, w - ML - MR);
  }

  setTitle(t, sub) {
    this.titleEl.textContent = t;
    if (sub !== undefined) this.subEl.textContent = sub;
  }

  setLoading(on) {
    this.el.classList.toggle("loading", !!on);
  }

  setExports(list) {
    clear(this.actions);
    for (const x of list || []) {
      this.actions.appendChild(h("a", { href: x.href, download: x.download || "", title: x.title || "Download what this chart shows" }, x.label));
    }
  }

  setError(msg) {
    this.data = null;
    this.error = msg;
    this.render();
  }

  setData(d) {
    this.error = null;
    this.data = d;
    this.render();
  }

  // Subclasses override these.
  yDomain() {
    return [0, 1];
  }
  drawData() {}
  valuesAt() {
    return [];
  }
  legendItems() {
    return (this.data && this.data.series || []).map((x) => ({ name: x.name, color: x.color, box: false, id: x.name }));
  }
  isEmpty() {
    const d = this.data;
    if (!d || !d.series || !d.series.length) return true;
    return !d.series.some((x) => x.v && x.v.some(isVal));
  }

  // tolerance is how far from a sample the crosshair may be and still
  // show its value.
  tolerance() {
    const d = this.data;
    if (!d) return 0;
    if (d.tol) return d.tol;
    if (d.step) return d.step * 0.75;
    return (d.to - d.from) / 40;
  }

  render() {
    const svg = this.svg;
    clear(svg);
    const W = this.width;
    if (!W) return;
    const H = this.o.height;
    svg.setAttribute("width", W);
    svg.setAttribute("height", H);
    svg.setAttribute("viewBox", "0 0 " + W + " " + H);
    const pw = W - ML - MR, ph = H - MT - MB;
    this.pw = pw;
    this.ph = ph;
    const d = this.data;
    if (!d) {
      svg.appendChild(s("text", { x: ML + pw / 2, y: MT + ph / 2, "text-anchor": "middle", class: "empty" }, this.error ? "Error: " + this.error : "Loading..."));
      clear(this.legendEl);
      return;
    }
    const x = linear(d.from, d.to, ML, ML + pw);
    let [lo, hi] = this.yDomain(d);
    if (!isNum(lo) || !isNum(hi)) [lo, hi] = [0, 1];
    if (this.o.zero) {
      lo = Math.min(0, lo);
      hi = Math.max(0, hi);
    }
    if (hi === lo) hi = lo + (Math.abs(lo) || 1);
    const ui = unitInfo(this.o.unit);
    let yt = niceTicks(lo, hi, Math.max(2, Math.floor(ph / 34)), ui.binary);
    if (this.o.integer && yt.step < 1) {
      // Counts take whole-number ticks.
      const top = Math.max(1, Math.ceil(hi));
      yt = { ticks: Array.from({ length: top + 1 }, (_, i) => i), step: 1, lo: 0, hi: top };
    }
    const y = linear(yt.lo, yt.hi, MT + ph, MT);
    this.x = x;
    this.y = y;

    const gBack = s("g");
    const gGrid = s("g");
    const gData = s("g");
    const gMark = s("g");
    const gCross = s("g");
    svg.append(gBack, gGrid, gData, gMark, gCross);

    // Grid and y axis.
    if (this.yAxis) {
      this.yAxis(gGrid, ML, MT, pw, ph);
    } else {
      const yfmt = axisFormat(this.o.unit, Math.max(Math.abs(yt.lo), Math.abs(yt.hi)), yt.step);
      let gp = "";
      for (const v of yt.ticks) {
        const py = Math.round(y(v)) + 0.5;
        gp += "M" + ML + " " + py + "H" + (ML + pw);
        gGrid.appendChild(s("text", { x: ML - 6, y: py + 3.5, "text-anchor": "end" }, yfmt(this.o.absTicks ? Math.abs(v) : v)));
      }
      gGrid.appendChild(s("path", { d: gp, class: "grid-line" }));
      if (yt.lo < 0 && yt.hi > 0) gGrid.appendChild(s("path", { d: "M" + ML + " " + (Math.round(y(0)) + 0.5) + "H" + (ML + pw), class: "zero-line" }));
    }
    // X axis.
    const tt = this.o.relative ? offsetTicks(d.from, d.to, Math.max(2, Math.floor(pw / 90))) : timeTicks(d.from, d.to, Math.max(2, Math.floor(pw / 90)));
    let tp = "M" + ML + " " + (MT + ph + 0.5) + "H" + (ML + pw);
    for (const t of tt.ticks) {
      const px = Math.round(x(t)) + 0.5;
      if (px < ML - 1 || px > ML + pw + 1) continue;
      tp += "M" + px + " " + (MT + ph) + "v4";
      gGrid.appendChild(s("text", { x: px, y: H - 5, "text-anchor": "middle" }, tt.label(t)));
    }
    gGrid.appendChild(s("path", { d: tp, class: "axis-line" }));

    this.drawRegions(gBack, x, MT, ph, d);
    this.drawTiers(gBack, x, MT, ph, d);

    if (this.isEmpty()) {
      gData.appendChild(s("text", { x: ML + pw / 2, y: MT + ph / 2, "text-anchor": "middle", class: "empty" }, this.o.emptyText || "No data in this range"));
    } else {
      this.drawData(gData, x, y, d);
    }
    this.drawMarkers(gMark, x, MT, ph, d);

    // Crosshair, selection and the hit area on top.
    this.crossLine = s("path", { class: "cross", d: "", visibility: "hidden" });
    this.dots = s("g");
    this.selRect = s("rect", { class: "selrect", x: 0, y: MT, width: 0, height: ph, visibility: "hidden" });
    const hit = s("rect", { class: "hit", x: ML, y: MT, width: pw, height: ph });
    gCross.append(this.crossLine, this.dots, this.selRect, hit);
    this.bindPointer(hit);

    this.renderLegend();
    this.renderFoot(d);
    if (this.bus && this.bus.t !== null) this.cross(this.bus.t, this.bus.source);
  }

  drawRegions(g, x, top, ph, d) {
    this.regionsShown = new Set();
    for (const r of this.ctx.regions || []) {
      const a = Math.max(d.from, r.from), b = Math.min(d.to, r.to);
      if (b <= a) continue;
      this.regionsShown.add(r.type);
      g.appendChild(s("rect", { x: x(a), y: top, width: Math.max(1, x(b) - x(a)), height: ph, class: "region-" + r.type }));
    }
    for (const a of this.ctx.annotations || []) {
      if (!(a.end > a.start)) continue;
      const p = Math.max(d.from, a.start), q = Math.min(d.to, a.end);
      if (q <= p) continue;
      g.appendChild(s("rect", { x: x(p), y: top, width: Math.max(1, x(q) - x(p)), height: ph, class: "ann-range" }));
    }
  }

  // drawTiers labels the coarser stretch when parts of the range came
  // from different tiers.
  drawTiers(g, x, top, ph, d) {
    const parts = (d.parts || []).filter((p) => p.to > d.from && p.from < d.to);
    this.tierNote = "";
    if (!parts.length) return;
    const tiers = [...new Set(parts.map((p) => p.tier))];
    if (tiers.length === 1) {
      if (tiers[0] !== "raw") this.tierNote = tiers[0] + " buckets";
      return;
    }
    const finest = Math.min(...tiers.map((t) => TIER_ORDER[t] ?? 0));
    for (const p of parts) {
      if ((TIER_ORDER[p.tier] ?? 0) <= finest) continue;
      const a = x(Math.max(d.from, p.from)), b = x(Math.min(d.to, p.to));
      if (b - a < 2) continue;
      g.appendChild(s("rect", { x: a, y: top, width: b - a, height: ph, class: "tier-shade" }));
      if (b - a > 50) g.appendChild(s("text", { x: a + 4, y: top + 10, class: "tier-label" }, p.tier + " buckets"));
    }
    this.tierNote = "mixed tiers: " + parts.map((p) => p.tier).join(", ");
  }

  // notable returns the events drawn as markers on this chart.
  markerEvents() {
    return this.ctx.events || [];
  }

  drawMarkers(g, x, top, ph, d) {
    const paths = {};
    this.marks = [];
    const add = (t, cls, ev) => {
      if (t < d.from || t > d.to) return;
      const px = Math.round(x(t)) + 0.5;
      paths[cls] = (paths[cls] || "") + "M" + px + " " + top + "V" + (top + ph);
      this.marks.push({ x: px, ev });
    };
    for (const ev of this.markerEvents()) {
      const cls = ev.sev === "critical" || ev.sev === "error" ? "error" : ev.sev === "warn" ? "warn" : "info";
      add(ev.t, cls, ev);
    }
    for (const a of this.ctx.annotations || []) {
      add(a.start, "annotation", { t: a.start, kind: "annotation", sev: "info", msg: a.text, node: a.node, id: a.id, end: a.end });
    }
    for (const [cls, p] of Object.entries(paths)) g.appendChild(s("path", { d: p, class: "marker " + cls }));
    this.marks.sort((a, b) => a.x - b.x);
  }

  markNear(px) {
    let best = null, bd = 5;
    for (const m of this.marks || []) {
      const dd = Math.abs(m.x - px);
      if (dd < bd) {
        bd = dd;
        best = m;
      }
    }
    return best;
  }

  bindPointer(hit) {
    let drag = null;
    const local = (e) => {
      const r = this.svg.getBoundingClientRect();
      return Math.max(ML, Math.min(ML + this.pw, e.clientX - r.left));
    };
    hit.addEventListener("pointerdown", (e) => {
      if (e.button !== 0) return;
      drag = { x0: local(e), moved: false };
      try {
        hit.setPointerCapture(e.pointerId);
      } catch (err) {
        /* synthetic events have no active pointer */
      }
    });
    hit.addEventListener("pointermove", (e) => {
      const px = local(e);
      const t = this.x.invert(px);
      if (drag) {
        if (Math.abs(px - drag.x0) > 4) drag.moved = true;
        if (drag.moved) {
          this.selRect.setAttribute("x", Math.min(px, drag.x0));
          this.selRect.setAttribute("width", Math.abs(px - drag.x0));
          this.selRect.setAttribute("visibility", "visible");
        }
      }
      if (this.bus) this.bus.move(t, this);
      else this.cross(t, this);
      const m = this.markNear(px);
      if (m) showTip(eventTip(m.ev), e.clientX, e.clientY);
      else showTip(this.valueTip(t), e.clientX, e.clientY);
    });
    const end = (e, cancel) => {
      if (!drag) return;
      const px = local(e);
      const d0 = drag;
      drag = null;
      this.selRect.setAttribute("visibility", "hidden");
      if (cancel) return;
      if (d0.moved && Math.abs(px - d0.x0) > 4) {
        const a = this.x.invert(Math.min(px, d0.x0)), b = this.x.invert(Math.max(px, d0.x0));
        if (this.bus) this.bus.zoom(a, b);
        else if (this.o.onZoom) this.o.onZoom(a, b);
        return;
      }
      const m = this.markNear(px);
      if (m && this.ctx.onEventClick) this.ctx.onEventClick(m.ev);
    };
    hit.addEventListener("pointerup", (e) => end(e, false));
    hit.addEventListener("pointercancel", (e) => end(e, true));
    hit.addEventListener("pointerleave", () => {
      if (drag) return;
      if (this.bus) this.bus.move(null, this);
      else this.cross(null, this);
      hideTip();
    });
  }

  valueTip(t) {
    const d = this.data;
    const rows = this.valuesAt(t);
    const step = d && d.step;
    const label = step ? fmtClock(t - step / 2, step < 60e3) + " - " + fmtClock(t + step / 2, step < 60e3) : fmtDateTime(t);
    const head = this.o.relative ? "+" + Math.round(t / 60000) + " min" : (step ? fmtDateTime(t - step / 2).slice(0, 11) + label : label);
    return h("div", null, h("div", { class: "tt-time" }, head),
      rows.map((r) => h("div", { class: "tt-row" }, h("span", { class: "key", style: { background: r.color } }), h("span", { class: "v" }, r.text), h("span", { class: "n" }, r.name))));
  }

  // cross draws the hairline and updates legend values at time t.
  cross(t, src) {
    if (!this.data || !this.x || !this.crossLine) return;
    const d = this.data;
    if (t === null || t < d.from || t > d.to) {
      this.crossLine.setAttribute("visibility", "hidden");
      clear(this.dots);
      this.updateLegend(null);
      return;
    }
    const px = Math.round(this.x(t)) + 0.5;
    this.crossLine.setAttribute("d", "M" + px + " " + MT + "V" + (MT + this.ph));
    this.crossLine.setAttribute("visibility", "visible");
    this.updateLegend(t);
    this.drawDots(t);
  }

  drawDots() {}

  renderLegend() {
    clear(this.legendEl);
    this.legendVals = new Map();
    for (const it of this.legendItems()) {
      const val = h("span", { class: "val" });
      const item = h("span", { class: "item" + (this.hiddenSeries.has(it.id) ? " off" : ""), title: it.title || "Click to hide or show" },
        h("span", { class: "key" + (it.box ? " box" : ""), style: { background: it.color } }), h("span", { class: "name" }, it.name), val);
      if (this.o.toggle !== false) {
        item.style.cursor = "pointer";
        item.addEventListener("click", () => {
          if (this.hiddenSeries.has(it.id)) this.hiddenSeries.delete(it.id);
          else this.hiddenSeries.add(it.id);
          this.render();
        });
      }
      this.legendVals.set(it.id, val);
      this.legendEl.appendChild(item);
    }
    this.updateLegend(null);
  }

  // updateLegend shows values at t, or the latest values when t is null.
  updateLegend(t) {
    if (!this.legendVals) return;
    const rows = t === null ? this.lastValues() : this.valuesAt(t);
    const by = new Map(rows.map((r) => [r.id, r.text]));
    for (const [id, el] of this.legendVals) el.textContent = by.has(id) ? by.get(id) : "";
  }

  lastValues() {
    const d = this.data;
    if (!d || !d.series) return [];
    const out = [];
    for (const x of d.series) {
      let v = null;
      for (let i = x.v.length - 1; i >= 0; i--) {
        if (isVal(x.v[i])) {
          v = x.v[i];
          break;
        }
      }
      out.push({ id: x.name, name: x.name, color: x.color, value: v, text: v === null ? "-" : fmt(v, this.o.unit) });
    }
    return out;
  }

  // seriesAt is the common valuesAt for series of {t, v}.
  seriesAt(t) {
    const d = this.data;
    if (!d || !d.series) return [];
    const tol = this.tolerance();
    const out = [];
    for (const x of d.series) {
      if (this.hiddenSeries.has(x.name)) continue;
      const i = bisect(x.t, t);
      let v = null;
      if (i >= 0 && Math.abs(x.t[i] - t) <= tol && isVal(x.v[i])) v = x.v[i];
      out.push({ id: x.name, name: x.name, color: x.color, value: v, i, text: v === null ? "-" : fmt(v, this.o.unit) });
    }
    return out;
  }

  renderFoot(d) {
    clear(this.footEl);
    const add = (cls, text) => this.footEl.appendChild(h("span", { class: cls }, text));
    if (this.regionsShown && this.regionsShown.has("monitor")) add("k-monitor", "algod.top was down");
    if (this.regionsShown && this.regionsShown.has("algod")) add("k-algod", "algod was down");
    if (this.tierNote) add("", this.tierNote);
    if (d.raw) add("", d.downsampled ? "full resolution, " + d.downsampled : "full resolution");
    for (const w of d.warnings || []) add("error", w);
    if (d.note) add("", d.note);
  }
}

export function unitLabel(u) {
  switch (u) {
    case "bytes": return "bytes";
    case "B/s": return "bytes/s";
    case "s": return "seconds";
    case "%": return "percent";
    case "": case undefined: return "";
    default: return u.replace(/^\//, "per ");
  }
}

// eventTip renders an event for the tooltip.
export function eventTip(ev) {
  return h("div", null,
    h("div", { class: "tt-time" }, fmtDateTime(ev.t) + (ev.node ? "  " + ev.node : "")),
    h("div", null, h("span", { class: "sev " + (ev.sev || "info") }, ev.kind)),
    h("div", { class: "tt-msg" }, ev.msg || ""),
    h("div", { class: "tt-time" }, "Click to show it in the events table"));
}
