// A panel is one chart plus the queries behind it. It asks /api/series for
// exactly as many points as the chart has pixels, converts the answer to
// chart series, and refreshes only the newest buckets while the range is
// live.

import { get, qs } from "./api.js";
import { LineChart } from "./chart/line.js";
import { AreaChart } from "./chart/area.js";
import { BarsChart } from "./chart/bars.js";
import { HeatmapChart } from "./chart/heatmap.js";
import { lttb } from "./chart/lttb.js";
import { insertGaps, isVal } from "./chart/frame.js";
import { slot, dirColor } from "./colors.js";

const TYPES = { line: LineChart, step: LineChart, area: AreaChart, bars: BarsChart, heatmap: HeatmapChart };

export function labelKey(l) {
  return Object.keys(l || {}).sort().map((k) => k + "=" + l[k]).join(",");
}

function labelName(l) {
  const ks = Object.keys(l || {}).filter((k) => k !== "method");
  if (!ks.length) return "value";
  return ks.sort().map((k) => l[k]).join(" ");
}

// alignGrid puts bucket edges on multiples of the step, so a later request
// for the newest buckets lines up with the ones already drawn.
export function alignGrid(from, to, points) {
  const step = Math.max(1, Math.ceil((to - from) / points));
  const end = Math.ceil(to / step) * step;
  return { from: end - points * step, to: end, points, step };
}

// binRaw averages raw samples onto a fixed grid, for stacked charts whose
// series must share one time axis.
export function binRaw(t, v, from, step, n) {
  const sum = new Float64Array(n), cnt = new Uint32Array(n);
  for (let i = 0; i < t.length; i++) {
    if (!isVal(v[i])) continue;
    const b = Math.floor((t[i] - from) / step);
    if (b < 0 || b >= n) continue;
    sum[b] += v[i];
    cnt[b]++;
  }
  const ot = new Array(n), ov = new Array(n);
  for (let i = 0; i < n; i++) {
    ot[i] = from + (i + 0.5) * step;
    ov[i] = cnt[i] ? sum[i] / cnt[i] : null;
  }
  return { t: ot, v: ov };
}

// bridge joins buckets across short runs of empty ones. When buckets are
// narrower than the sampling interval (a 10 s scrape on a 1 h chart with 5 s
// buckets), every other bucket is empty; that is not a gap. A run of empty
// buckets longer than 2.5 times the usual spacing stays a gap. Lines drop
// the bridged buckets; stacked charts fill them by interpolation, because
// their series must keep one shared grid.
export function bridge(sr, interpolate) {
  const idx = [];
  for (let i = 0; i < sr.v.length; i++) if (isVal(sr.v[i])) idx.push(i);
  if (idx.length < 3) return 1;
  const gaps = [];
  for (let k = 1; k < idx.length; k++) gaps.push(idx[k] - idx[k - 1]);
  gaps.sort((a, b) => a - b);
  const g = gaps[gaps.length >> 1];
  if (g <= 1) return 1;
  const lim = Math.ceil(g * 2.5);
  const drop = new Uint8Array(sr.v.length);
  for (let k = 1; k < idx.length; k++) {
    const a = idx[k - 1], b = idx[k];
    if (b - a <= 1 || b - a > lim) continue;
    for (let i = a + 1; i < b; i++) {
      if (interpolate) {
        const f = (i - a) / (b - a);
        sr.v[i] = sr.v[a] + (sr.v[b] - sr.v[a]) * f;
        if (sr.min && isVal(sr.min[a]) && isVal(sr.min[b])) sr.min[i] = sr.min[a] + (sr.min[b] - sr.min[a]) * f;
        if (sr.max && isVal(sr.max[a]) && isVal(sr.max[b])) sr.max[i] = sr.max[a] + (sr.max[b] - sr.max[a]) * f;
      } else drop[i] = 1;
    }
  }
  if (!interpolate) {
    const keep = (a) => (a ? a.filter((_, i) => !drop[i]) : a);
    sr.min = keep(sr.min);
    sr.max = keep(sr.max);
    sr.t = keep(sr.t);
    sr.v = keep(sr.v);
  }
  return g;
}

export function seriesParams(q, view, grid) {
  return {
    node: q.node || view.node,
    metric: q.metric,
    from: Math.floor(grid.from),
    to: Math.ceil(grid.to),
    points: grid.points,
    tier: view.raw ? "raw" : q.tier,
    fn: q.fn,
    by: q.by,
    match: q.match,
    limit: q.limit,
  };
}

export class Panel {
  constructor(spec, page) {
    this.spec = spec;
    this.page = page;
    const C = TYPES[spec.type || "line"] || LineChart;
    this.chart = new C({
      title: spec.title, unit: spec.unit || "", sub: spec.sub, bus: page.bus, ctx: page.ctx,
      band: spec.band, step: spec.type === "step", zero: spec.zero !== false, height: spec.height,
      relative: page.relative, cellUnit: spec.cellUnit, emptyText: spec.emptyText, unitLabel: spec.unitLabel,
    });
    this.el = this.chart.el;
    if (spec.wide) this.el.style.gridColumn = "1 / -1";
    this.colors = new Map();
    this.loadedKey = null;
    this.loading = false;
    this.lastLoad = 0;
    this.visible = false;
    this.seq = 0;
  }

  destroy() {
    this.chart.destroy();
  }

  colorFor(key, q, labels, idx) {
    if (q && q.color) return typeof q.color === "function" ? q.color(labels, idx) : q.color;
    // Direction keeps its fixed colour on every chart, the Explorer included.
    const dir = labels && labels.dir;
    if (dir === "in" || dir === "out" || dir === "inbound" || dir === "outbound") return dirColor(dir, labels.net);
    if (!this.colors.has(key)) this.colors.set(key, slot(this.colors.size));
    return this.colors.get(key);
  }

  // refreshInterval is how often a live panel refetches its newest part.
  refreshInterval() {
    const d = this.chart.data;
    const step = d && d.step ? d.step : 0;
    return Math.max(10e3, Math.min(300e3, step || 15e3));
  }

  async load(view, opts = {}) {
    const my = ++this.seq;
    this.loading = true;
    this.chart.setLoading(true);
    await this.chart.ready;
    let pts = Math.max(20, Math.min(2000, Math.round(this.chart.plotWidth())));
    // Slowly sampled metrics ask for buckets no narrower than their interval.
    if (this.spec.minStep && !view.raw) pts = Math.max(2, Math.min(pts, Math.floor((view.to - view.from) / this.spec.minStep)));
    try {
      let data;
      if (opts.tail && this.canTail(view)) {
        data = await this.tail(view);
      }
      if (!data) {
        if (this.spec.load) {
          data = await this.spec.load(view, pts, this);
        } else {
          const grid = view.live && !view.raw ? alignGrid(view.from, view.to, pts) : { from: view.from, to: view.to, points: pts, step: (view.to - view.from) / pts };
          const res = await Promise.all(this.spec.q.map((q) => get("/api/series", seriesParams(q, view, grid))));
          data = this.toData(res, grid, view);
          this.grid = grid;
          this.setExports(view, grid);
        }
      }
      if (my !== this.seq) return;
      this.loadedKey = view.key;
      this.lastLoad = Date.now();
      this.chart.setData(data);
    } catch (e) {
      if (my !== this.seq) return;
      this.chart.setError(e.message || String(e));
    } finally {
      if (my === this.seq) {
        this.loading = false;
        this.chart.setLoading(false);
      }
    }
  }

  setExports(view, grid) {
    if (this.spec.noExport) return;
    const list = [];
    this.spec.q.forEach((q, i) => {
      const p = seriesParams(q, view, grid);
      const tag = this.spec.q.length > 1 ? " " + (q.exportLabel || i + 1) : "";
      list.push({ label: "CSV" + tag, href: "/api/export" + qs(Object.assign({}, p, { format: "csv" })), title: "CSV of " + q.metric });
      list.push({ label: "JSON" + tag, href: "/api/export" + qs(Object.assign({}, p, { format: "json" })), title: "JSON of " + q.metric });
    });
    this.chart.setExports(list);
  }

  toData(results, grid, view) {
    const spec = this.spec;
    let series = [];
    const parts = [];
    const seen = new Set();
    const warnings = [];
    let raw = false, step = 0, down = 0;
    results.forEach((res, qi) => {
      const q = spec.q[qi];
      raw = raw || !!res.raw;
      if (!res.raw) step = res.step / 1e6;
      for (const p of res.parts || []) {
        const k = p.from + p.tier + p.to;
        if (!seen.has(k)) {
          seen.add(k);
          parts.push({ from: p.from / 1e6, to: p.to / 1e6, tier: p.tier });
        }
      }
      for (const w of res.warnings || []) warnings.push(w);
      (res.series || []).forEach((sr, si) => {
        const labels = sr.labels || {};
        if (q.filter && !q.filter(labels)) return;
        const k = q.scale || 1;
        const sc = (a) => (a ? a.map((x) => (x === null ? null : x * k)) : null);
        const half = res.raw ? 0 : res.step / 2e6;
        let t = sr.t.map((x) => x / 1e6 + half);
        let v = sc(sr.avg);
        let mn = sc(sr.min), mx = sc(sr.max);
        if (res.raw) {
          const g = insertGaps(t, v, 4);
          t = g.t;
          v = g.v;
          mn = mx = null;
          if (spec.type === "area" || spec.type === "bars") {
            const b = binRaw(t, v, grid.from, grid.step, grid.points);
            t = b.t;
            v = b.v;
            step = grid.step;
          } else if (t.length > grid.points * 1.2) {
            down += t.length;
            const r = lttb(t, v, grid.points);
            t = r.t;
            v = r.v;
          }
        }
        const key = (q.id || q.metric) + "|" + labelKey(labels);
        series.push({
          key, labels, q: qi, t, v, min: mn, max: mx,
          name: q.name ? (typeof q.name === "function" ? q.name(labels, sr) : q.name) : labelName(labels),
          color: this.colorFor(key, q, labels, si),
          neg: q.neg ? (typeof q.neg === "function" ? q.neg(labels) : true) : false,
          dash: q.dash,
        });
      });
    });
    if (spec.derive) series = spec.derive(series, this);
    let spacing = 1;
    if (!raw && spec.type !== "bars") {
      for (const sr of series) spacing = Math.max(spacing, bridge(sr, spec.type === "area"));
    }
    if (spec.sort) series.sort(spec.sort);
    const d = { from: grid.from, to: grid.to, step: raw && !(spec.type === "area" || spec.type === "bars") ? 0 : step, raw, parts, series, warnings };
    if (spacing > 1 && d.step) d.tol = d.step * spacing * 0.75;
    if (raw && down) d.downsampled = "LTTB to " + grid.points + " points from " + down;
    if (raw && !d.step) {
      // Crosshair tolerance for irregular raw samples.
      d.tol = Math.max(15e3, (grid.to - grid.from) / 200);
    }
    if (spec.note) d.note = typeof spec.note === "function" ? spec.note(series, results) : spec.note;
    return d;
  }

  canTail(view) {
    const d = this.chart.data;
    return !!(d && this.grid && view.live && !view.raw && !this.spec.load && !this.spec.derive && this.loadedKey === view.key && d.step > 0);
  }

  // tail fetches only the buckets from the last drawn one to now, and
  // shifts the window forward.
  async tail(view) {
    const g = this.grid;
    const old = this.chart.data;
    const step = g.step;
    const end = Math.ceil(Date.now() / step) * step;
    const start = end - g.points * step;
    const tf = g.to - step;
    const n = Math.max(1, Math.round((end - tf) / step));
    const tg = { from: tf, to: tf + n * step, points: n, step };
    const res = await Promise.all(this.spec.q.map((q) => get("/api/series", seriesParams(q, view, tg))));
    const fresh = this.toData(res, tg, view);
    const byKey = new Map(fresh.series.map((x) => [x.key, x]));
    const out = [];
    for (const sr of old.series) {
      const nw = byKey.get(sr.key);
      byKey.delete(sr.key);
      const keep = [];
      for (let i = 0; i < sr.t.length; i++) if (sr.t[i] >= start && sr.t[i] < tf) keep.push(i);
      const pick = (a) => (a ? keep.map((i) => a[i]) : null);
      const t = pick(sr.t), v = pick(sr.v);
      const mn = pick(sr.min), mx = pick(sr.max);
      const grid = this.spec.type === "area" || this.spec.type === "bars";
      if (grid) {
        // Stacked series keep one full grid.
        for (let i = 0; i < n; i++) {
          t.push(tf + (i + 0.5) * step);
          v.push(nw && i < nw.v.length ? nw.v[i] : null);
          if (mn) mn.push(nw && nw.min ? nw.min[i] : null);
          if (mx) mx.push(nw && nw.max ? nw.max[i] : null);
        }
      } else if (nw) {
        // Leading empty buckets sit between samples, not in a gap.
        let i0 = 0;
        while (i0 < nw.v.length && !isVal(nw.v[i0])) i0++;
        for (let i = i0; i < nw.t.length; i++) {
          t.push(nw.t[i]);
          v.push(nw.v[i]);
          if (mn) mn.push(nw.min ? nw.min[i] : null);
          if (mx) mx.push(nw.max ? nw.max[i] : null);
        }
      }
      const merged = Object.assign({}, sr, { t, v, min: mn, max: mx });
      if (this.spec.type === "area") bridge(merged, true);
      out.push(merged);
    }
    if (byKey.size) return null; // a new series appeared: load it all
    const parts = old.parts.filter((p) => p.to > start).map((p) => Object.assign({}, p));
    for (const p of fresh.parts) {
      const last = parts[parts.length - 1];
      if (last && last.tier === p.tier && p.from <= last.to + step) last.to = Math.max(last.to, p.to);
      else parts.push(p);
    }
    if (parts.length) parts[0].from = Math.max(parts[0].from, start);
    this.grid = { from: start, to: end, points: g.points, step };
    this.setExports(view, this.grid);
    return Object.assign({}, old, { from: start, to: end, series: out, parts, warnings: fresh.warnings });
  }
}
