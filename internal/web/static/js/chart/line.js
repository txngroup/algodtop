// Line and step charts, with an optional min/max band behind the average
// for gauges over long ranges. One SVG path per series; null values break
// the line so gaps stay gaps.

import { s } from "../dom.js";
import { Chart, isVal } from "./frame.js";

// linePath builds one path for a series. With step set, each value holds
// until the next sample (raw) or spans its bucket (half a step each side).
export function linePath(t, v, x, y, step, half) {
  let p = "";
  let pen = false;
  let runLen = 0;
  const n = t.length;
  for (let i = 0; i < n; i++) {
    const val = v[i];
    if (!isVal(val)) {
      if (pen && runLen === 1 && !step) p += "h0.5";
      pen = false;
      runLen = 0;
      continue;
    }
    const px = x(t[i]), py = y(val);
    if (step) {
      const a = half ? x(t[i] - half) : px;
      const b = half ? x(t[i] + half) : (i + 1 < n && isVal(v[i + 1]) ? x(t[i + 1]) : px);
      if (!pen) p += "M" + a.toFixed(1) + " " + py.toFixed(1);
      else p += "H" + a.toFixed(1) + "V" + py.toFixed(1);
      p += "H" + Math.max(a + 0.5, b).toFixed(1);
    } else {
      p += (pen ? "L" : "M") + px.toFixed(1) + " " + py.toFixed(1);
    }
    pen = true;
    runLen++;
  }
  if (pen && runLen === 1 && !step) p += "h0.5";
  return p;
}

// bandPath builds the min/max band, one closed shape per run.
export function bandPath(t, lo, hi, x, y) {
  let p = "";
  let run = [];
  const flush = () => {
    if (run.length > 1) {
      p += "M" + run.map((i) => x(t[i]).toFixed(1) + " " + y(hi[i]).toFixed(1)).join("L");
      p += "L" + run.slice().reverse().map((i) => x(t[i]).toFixed(1) + " " + y(lo[i]).toFixed(1)).join("L") + "Z";
    }
    run = [];
  };
  for (let i = 0; i < t.length; i++) {
    if (isVal(lo[i]) && isVal(hi[i])) run.push(i);
    else flush();
  }
  flush();
  return p;
}

export class LineChart extends Chart {
  yDomain(d) {
    let lo = Infinity, hi = -Infinity;
    for (const x of d.series) {
      if (this.hiddenSeries.has(x.name)) continue;
      const band = this.o.band && x.min && x.max;
      for (let i = 0; i < x.v.length; i++) {
        const v = x.v[i];
        if (isVal(v)) {
          if (v < lo) lo = v;
          if (v > hi) hi = v;
        }
        if (band) {
          if (isVal(x.min[i]) && x.min[i] < lo) lo = x.min[i];
          if (isVal(x.max[i]) && x.max[i] > hi) hi = x.max[i];
        }
      }
    }
    if (lo === Infinity) return [0, 1];
    if (!this.o.zero) {
      const pad = (hi - lo) * 0.08 || Math.abs(hi) * 0.05 || 1;
      return [lo >= 0 ? Math.max(0, lo - pad) : lo - pad, hi + pad];
    }
    return [lo, hi * 1.05];
  }

  drawData(g, x, y, d) {
    const half = this.o.step && d.step ? d.step / 2 : 0;
    // Bands first, so every line draws on top of every band.
    if (this.o.band) {
      for (const sr of d.series) {
        if (this.hiddenSeries.has(sr.name) || !sr.min || !sr.max) continue;
        const p = bandPath(sr.t, sr.min, sr.max, x, y);
        if (p) {
          const el = s("path", { d: p, class: "band" });
          el.style.fill = sr.color;
          g.appendChild(el);
        }
      }
    }
    for (const sr of d.series) {
      if (this.hiddenSeries.has(sr.name)) continue;
      const el = s("path", { d: linePath(sr.t, sr.v, x, y, this.o.step || sr.step, half), class: "series" });
      el.style.stroke = sr.color;
      if (sr.dash) el.setAttribute("stroke-dasharray", "4 3");
      g.appendChild(el);
    }
  }

  valuesAt(t) {
    return this.seriesAt(t);
  }

  drawDots(t) {
    while (this.dots.firstChild) this.dots.removeChild(this.dots.firstChild);
    for (const r of this.seriesAt(t)) {
      if (r.value === null) continue;
      const sr = this.data.series.find((q) => q.name === r.id);
      const c = s("circle", { cx: this.x(this.o.step ? t : sr.t[r.i]), cy: this.y(r.value), r: 3.5, class: "cross-dot" });
      c.style.fill = r.color;
      this.dots.appendChild(c);
    }
  }
}
