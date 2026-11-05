// Scales and ticks shared by every chart type: linear number and time
// scales, "nice" 1-2-5 ticks (binary-aware for byte sizes), and time ticks
// aligned to local clock boundaries.

import { fmtClock, fmtDay, fmtMonth, fmtDur, prefixFor } from "../format.js";

// linear maps [d0, d1] onto [r0, r1].
export function linear(d0, d1, r0, r1) {
  const span = d1 - d0 || 1;
  const k = (r1 - r0) / span;
  const f = (v) => r0 + (v - d0) * k;
  f.invert = (p) => d0 + (p - r0) / k;
  f.domain = [d0, d1];
  f.range = [r0, r1];
  return f;
}

function niceStep(raw) {
  const p = Math.pow(10, Math.floor(Math.log10(raw)));
  const m = raw / p;
  const n = m <= 1 ? 1 : m <= 2 ? 2 : m <= 2.5 ? 2.5 : m <= 5 ? 5 : 10;
  return n * p;
}

// niceTicks returns about count ticks covering [lo, hi] at a 1-2-5 step.
// With binary set, the step is nice in the binary prefix of the range
// (0.5 GiB rather than 536.9 MB).
export function niceTicks(lo, hi, count, binary) {
  if (!(hi > lo)) {
    if (hi === lo) {
      const d = Math.abs(hi) || 1;
      lo -= d * 0.5;
      hi += d * 0.5;
    } else return { ticks: [0], step: 1, lo: 0, hi: 1 };
  }
  let div = 1;
  if (binary) div = prefixFor(Math.max(Math.abs(lo), Math.abs(hi)), true)[0];
  const l = lo / div, u = hi / div;
  const step = niceStep((u - l) / Math.max(1, count));
  const start = Math.floor(l / step + 1e-9) * step;
  const end = Math.ceil(u / step - 1e-9) * step;
  const ticks = [];
  for (let v = start; v <= end + step * 1e-6; v += step) ticks.push(Math.abs(v) < step * 1e-9 ? 0 : v * div);
  return { ticks, step: step * div, lo: start * div, hi: end * div };
}

const SEC = 1e3, MIN = 60e3, HOUR = 3600e3, DAY = 86400e3;
const INTERVALS = [
  SEC, 2 * SEC, 5 * SEC, 10 * SEC, 15 * SEC, 30 * SEC,
  MIN, 2 * MIN, 5 * MIN, 10 * MIN, 15 * MIN, 30 * MIN,
  HOUR, 2 * HOUR, 3 * HOUR, 6 * HOUR, 12 * HOUR,
  DAY, 2 * DAY, 7 * DAY, 14 * DAY,
];

// timeTicks returns ticks for a time range in ms, aligned to local
// boundaries, and a label function.
export function timeTicks(t0, t1, count) {
  const span = t1 - t0;
  const target = span / Math.max(1, count);
  const out = [];
  if (target > 20 * DAY) {
    // Months or years.
    const months = target > 300 * DAY ? 12 : target > 80 * DAY ? 3 : 1;
    const d = new Date(t0);
    d.setDate(1);
    d.setHours(0, 0, 0, 0);
    d.setMonth(Math.floor(d.getMonth() / months) * months);
    while (d.getTime() <= t1) {
      if (d.getTime() >= t0) out.push(d.getTime());
      d.setMonth(d.getMonth() + months);
    }
    const yearly = months === 12;
    return { ticks: out, label: (t) => (yearly ? String(new Date(t).getFullYear()) : fmtMonth(t)) };
  }
  let iv = INTERVALS[INTERVALS.length - 1];
  for (const x of INTERVALS) {
    if (x >= target) {
      iv = x;
      break;
    }
  }
  // Align to local midnight so ticks fall on round clock times.
  const mid = new Date(t0);
  mid.setHours(0, 0, 0, 0);
  const base = mid.getTime();
  let t = base + Math.ceil((t0 - base) / iv) * iv;
  if (iv >= DAY) {
    const d = new Date(t0);
    d.setHours(0, 0, 0, 0);
    if (d.getTime() < t0) d.setDate(d.getDate() + 1);
    const step = Math.round(iv / DAY);
    while (d.getTime() <= t1) {
      out.push(d.getTime());
      d.setDate(d.getDate() + step);
    }
    return { ticks: out, label: (x) => fmtDay(x) };
  }
  for (; t <= t1; t += iv) out.push(t);
  const secs = iv < MIN;
  return {
    ticks: out,
    label: (x) => {
      const d = new Date(x);
      if (d.getHours() === 0 && d.getMinutes() === 0 && d.getSeconds() === 0 && span > 6 * HOUR) return fmtDay(x);
      return fmtClock(x, secs);
    },
  };
}

// offsetTicks are ticks for a relative time axis (Compare), in ms from 0.
export function offsetTicks(t0, t1, count) {
  const target = (t1 - t0) / Math.max(1, count);
  let iv = INTERVALS[INTERVALS.length - 1];
  for (const x of INTERVALS) {
    if (x >= target) {
      iv = x;
      break;
    }
  }
  const out = [];
  for (let t = Math.ceil(t0 / iv) * iv; t <= t1; t += iv) out.push(t);
  return { ticks: out, label: (x) => (x === 0 ? "0" : "+" + fmtDur(x / 1000).replace(/ 0[ms]$/, "").replace(/\.0s$/, "s")) };
}

// bisect finds the index of the value in sorted xs nearest to x.
export function bisect(xs, x) {
  let lo = 0, hi = xs.length - 1;
  if (hi < 0) return -1;
  while (hi - lo > 1) {
    const m = (lo + hi) >> 1;
    if (xs[m] < x) lo = m;
    else hi = m;
  }
  return Math.abs(xs[lo] - x) <= Math.abs(xs[hi] - x) ? lo : hi;
}
