// Stacked bars per bucket, for event counts such as disconnect reasons per
// 10 minutes. One path per series holds every bar of that series.

import { s } from "../dom.js";
import { Chart, isVal, defined } from "./frame.js";

export class BarsChart extends Chart {
  constructor(opts) {
    super(Object.assign({ integer: true }, defined(opts)));
  }

  yDomain(d) {
    const n = d.series.length ? d.series[0].t.length : 0;
    let hi = 0;
    for (let i = 0; i < n; i++) {
      let sum = 0;
      for (const sr of d.series) if (!this.hiddenSeries.has(sr.name) && isVal(sr.v[i])) sum += sr.v[i];
      if (sum > hi) hi = sum;
    }
    return [0, hi || 1];
  }

  drawData(g, x, y, d) {
    const n = d.series.length ? d.series[0].t.length : 0;
    const half = d.step / 2;
    const acc = new Float64Array(n);
    for (const sr of d.series) {
      if (this.hiddenSeries.has(sr.name)) continue;
      let p = "";
      for (let i = 0; i < n; i++) {
        const v = sr.v[i];
        if (!isVal(v) || v <= 0) continue;
        let a = x(sr.t[i] - half), b = x(sr.t[i] + half);
        if (b - a > 3) {
          a += 0.5;
          b -= 0.5;
        }
        const y0 = y(acc[i]), y1 = y(acc[i] + v);
        acc[i] += v;
        p += "M" + a.toFixed(1) + " " + y0.toFixed(1) + "V" + y1.toFixed(1) + "H" + b.toFixed(1) + "V" + y0.toFixed(1) + "Z";
      }
      if (!p) continue;
      const el = s("path", { d: p, class: "bars" });
      el.style.fill = sr.color;
      g.appendChild(el);
    }
  }

  isEmpty() {
    const d = this.data;
    return !d || !d.series.length || !d.series.some((x) => x.v.some((v) => v > 0));
  }

  legendItems() {
    return (this.data && this.data.series || []).map((x) => ({ name: x.name, color: x.color, box: true, id: x.name }));
  }

  valuesAt(t) {
    return this.seriesAt(t);
  }

  lastValues() {
    // Totals over the range read better than the last bucket for counts.
    const d = this.data;
    if (!d) return [];
    return d.series.map((x) => {
      const tot = x.v.reduce((a, v) => a + (isVal(v) ? v : 0), 0);
      return { id: x.name, name: x.name, color: x.color, value: tot, text: String(Math.round(tot)) };
    });
  }
}
