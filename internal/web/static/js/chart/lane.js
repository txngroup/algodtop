// The event lane sits above the first chart and shares its x axis. Each
// event is a labelled mark; hovering shows it and clicking filters the
// events table. The Events page uses it as its timeline strip.

import { h, s, clear, showTip, hideTip } from "../dom.js";
import { linear, timeTicks } from "./scale.js";
import { ML, MR, eventTip } from "./frame.js";

const SYM = { info: "\u25CF", warn: "\u25B2", error: "\u25A0", critical: "\u2716", annotation: "\u25C6" };

export class Lane {
  constructor(opts) {
    this.o = Object.assign({ height: 46, axis: false }, opts);
    this.bus = this.o.bus;
    this.ctx = this.o.ctx || {};
    this.svg = s("svg", { class: "plot", height: this.o.height });
    this.el = h("div", { class: "lane" }, this.svg);
    this.width = 0;
    this.range = null;
    this.ro = new ResizeObserver((en) => {
      const w = Math.floor(en[0].contentRect.width);
      if (w > 0 && w !== this.width) {
        this.width = w;
        this.render();
      }
    });
    this.ro.observe(this.el);
    this.unsub = this.bus ? this.bus.on((t) => this.cross(t)) : null;
  }

  destroy() {
    this.ro.disconnect();
    if (this.unsub) this.unsub();
  }

  setRange(from, to) {
    this.range = { from, to };
    this.render();
  }

  items() {
    const out = [];
    for (const ev of this.ctx.events || []) out.push(ev);
    for (const a of this.ctx.annotations || []) out.push({ t: a.start, end: a.end, kind: "annotation", sev: "annotation", msg: a.text, node: a.node, id: a.id });
    return out.sort((a, b) => a.t - b.t);
  }

  render() {
    const svg = this.svg;
    clear(svg);
    const W = this.width;
    if (!(W > 0) || !this.range) return;
    const H = this.o.height + (this.o.axis ? 14 : 0);
    svg.setAttribute("width", W);
    svg.setAttribute("height", H);
    svg.setAttribute("viewBox", "0 0 " + W + " " + H);
    const pw = W - ML - MR;
    const { from, to } = this.range;
    const x = linear(from, to, ML, ML + pw);
    this.x = x;
    svg.appendChild(s("text", { x: ML - 6, y: 14, "text-anchor": "end" }, "events"));
    // Down-time regions, the same hatching as the charts.
    for (const r of this.ctx.regions || []) {
      const a = Math.max(from, r.from), b = Math.min(to, r.to);
      if (b > a) svg.appendChild(s("rect", { x: x(a), y: 2, width: Math.max(1, x(b) - x(a)), height: this.o.height - 4, class: "region-" + r.type }));
    }
    const rows = [ML, ML];
    this.marks = [];
    const list = this.items().filter((e) => e.t >= from && e.t <= to);
    let p = "";
    for (const ev of list) {
      const px = Math.round(x(ev.t)) + 0.5;
      p += "M" + px + " 2V" + (this.o.height - 2);
      this.marks.push({ x: px, ev });
    }
    if (p) svg.appendChild(s("path", { d: p, class: "marker" }));
    // Labels: two rows, and a label is skipped when it would overlap.
    for (const m of this.marks) {
      const ev = m.ev;
      const sev = ev.sev === "annotation" ? "annotation" : ev.sev === "critical" ? "critical" : ev.sev === "error" ? "error" : ev.sev === "warn" ? "warn" : "info";
      const label = (SYM[sev] || SYM.info) + " " + (ev.kind === "annotation" ? (ev.msg || "").slice(0, 24) : ev.kind);
      const wpx = label.length * 5.6 + 6;
      const row = rows[0] <= m.x ? 0 : rows[1] <= m.x ? 1 : -1;
      const sym = s("text", { x: m.x - 3, y: row === 1 ? 34 : 16, class: "lane-mark" }, row >= 0 && m.x + wpx < ML + pw + 40 ? label : (SYM[sev] || SYM.info));
      sym.style.fill = sev === "annotation" ? "var(--s1)" : sev === "warn" ? "var(--warn)" : sev === "info" ? "var(--ink2)" : "var(--crit)";
      if (row < 0) sym.setAttribute("y", 25);
      svg.appendChild(sym);
      if (row >= 0) rows[row] = m.x + wpx;
    }
    if (!list.length) svg.appendChild(s("text", { x: ML + 6, y: 26 }, "no notable events in this range"));
    if (this.o.axis) {
      const tt = timeTicks(from, to, Math.max(2, Math.floor(pw / 90)));
      for (const t of tt.ticks) svg.appendChild(s("text", { x: x(t), y: H - 2, "text-anchor": "middle" }, tt.label(t)));
    }
    this.crossLine = s("path", { class: "cross", visibility: "hidden" });
    svg.appendChild(this.crossLine);
    const hit = s("rect", { class: "hit", x: ML, y: 0, width: pw, height: this.o.height });
    svg.appendChild(hit);
    const near = (e) => {
      const r = svg.getBoundingClientRect();
      const px = e.clientX - r.left;
      let best = null, bd = 6;
      for (const m of this.marks) {
        const d = Math.abs(m.x - px);
        if (d < bd) {
          bd = d;
          best = m;
        }
      }
      return { px, best };
    };
    hit.addEventListener("pointermove", (e) => {
      const { px, best } = near(e);
      if (this.bus) this.bus.move(x.invert(px), this);
      if (best) showTip(eventTip(best.ev), e.clientX, e.clientY);
      else hideTip();
    });
    hit.addEventListener("pointerleave", () => {
      if (this.bus) this.bus.move(null, this);
      hideTip();
    });
    hit.addEventListener("click", (e) => {
      const { best } = near(e);
      if (best && this.ctx.onEventClick) this.ctx.onEventClick(best.ev);
    });
    this.cross(this.bus ? this.bus.t : null);
  }

  cross(t) {
    if (!this.crossLine || !this.range) return;
    if (t === null || t < this.range.from || t > this.range.to) {
      this.crossLine.setAttribute("visibility", "hidden");
      return;
    }
    const px = Math.round(this.x(t)) + 0.5;
    this.crossLine.setAttribute("d", "M" + px + " 0V" + this.o.height);
    this.crossLine.setAttribute("visibility", "visible");
  }
}
