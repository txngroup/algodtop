// Sparklines for fleet cards and table cells: no axes, y from zero, one
// path per series, gaps kept. The SVG scales to its box.

import { s } from "../dom.js";
import { linear } from "./scale.js";
import { linePath } from "./line.js";
import { isVal } from "./frame.js";

// spark draws series [{t, v, color}] over [from, to] into a new <svg>.
export function spark(series, from, to, opts = {}) {
  const W = opts.width || 120, H = opts.height || 32;
  const el = s("svg", { class: "spark", viewBox: "0 0 " + W + " " + H, preserveAspectRatio: "none", role: "img" });
  if (opts.label) el.appendChild(s("title", null, opts.label));
  let hi = 0;
  for (const x of series) for (const v of x.v) if (isVal(v) && v > hi) hi = v;
  if (!(hi > 0)) hi = 1;
  const xs = linear(from, to, 1, W - 1);
  const ys = linear(0, hi * 1.1, H - 1.5, 1.5);
  el.appendChild(s("path", { d: "M0 " + (H - 0.5) + "H" + W, class: "axis-line", "vector-effect": "non-scaling-stroke" }));
  for (const x of series) {
    const p = s("path", { d: linePath(x.t, x.v, xs, ys, false, 0), "vector-effect": "non-scaling-stroke" });
    p.style.fill = "none";
    p.style.stroke = x.color;
    p.style.strokeWidth = "1.5";
    p.style.strokeLinejoin = "round";
    p.style.strokeLinecap = "round";
    el.appendChild(p);
  }
  return el;
}
