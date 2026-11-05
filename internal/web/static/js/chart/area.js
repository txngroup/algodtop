// Stacked area chart. Series with neg set stack downward from zero, so
// inbound can sit above the axis and outbound below it in one chart; tick
// labels then show absolute values. All series share one time grid.

import { s } from "../dom.js";
import { Chart, isVal, defined } from "./frame.js";

export class AreaChart extends Chart {
  constructor(opts) {
    super(Object.assign({ absTicks: true }, defined(opts)));
  }

  stacks() {
    const d = this.data;
    const vis = d.series.filter((x) => !this.hiddenSeries.has(x.name));
    const n = vis.length ? vis[0].t.length : 0;
    const pos = new Float64Array(n), neg = new Float64Array(n);
    const out = [];
    for (const sr of vis) {
      const base = new Float64Array(n), top = new Float64Array(n);
      for (let i = 0; i < n; i++) {
        const v = sr.v[i];
        const acc = sr.neg ? neg : pos;
        base[i] = acc[i];
        if (isVal(v)) acc[i] += sr.neg ? -Math.abs(v) : v;
        top[i] = acc[i];
      }
      out.push({ sr, base, top });
    }
    return { out, pos, neg, n };
  }

  yDomain() {
    const { pos, neg, n } = this.stacks();
    let lo = 0, hi = 0;
    for (let i = 0; i < n; i++) {
      if (pos[i] > hi) hi = pos[i];
      if (neg[i] < lo) lo = neg[i];
    }
    return [lo * 1.05, hi * 1.05 || (lo < 0 ? 0 : 1)];
  }

  drawData(g, x, y) {
    const { out } = this.stacks();
    for (const { sr, base, top } of out) {
      let p = "";
      let run = [];
      const flush = () => {
        if (run.length === 1) {
          // A lone sample between gaps draws as a thin bar.
          const i = run[0], px = x(sr.t[i]);
          p += "M" + (px - 1).toFixed(1) + " " + y(top[i]).toFixed(1) + "H" + (px + 1).toFixed(1) + "V" + y(base[i]).toFixed(1) + "H" + (px - 1).toFixed(1) + "Z";
        } else if (run.length > 1) {
          p += "M" + run.map((i) => x(sr.t[i]).toFixed(1) + " " + y(top[i]).toFixed(1)).join("L");
          p += "L" + run.slice().reverse().map((i) => x(sr.t[i]).toFixed(1) + " " + y(base[i]).toFixed(1)).join("L") + "Z";
        }
        run = [];
      };
      for (let i = 0; i < sr.t.length; i++) {
        if (isVal(sr.v[i])) run.push(i);
        else flush();
      }
      flush();
      if (!p) continue;
      const el = s("path", { d: p, class: "area" });
      el.style.fill = sr.color;
      g.appendChild(el);
    }
  }

  legendItems() {
    return (this.data && this.data.series || []).map((x) => ({ name: x.name, color: x.color, box: true, id: x.name }));
  }

  valuesAt(t) {
    return this.seriesAt(t).map((r) => Object.assign(r, { text: r.value === null ? "-" : r.text.replace(/^-/, "") }));
  }

  lastValues() {
    return super.lastValues().map((r) => Object.assign(r, { text: r.text.replace(/^-/, "") }));
  }
}
