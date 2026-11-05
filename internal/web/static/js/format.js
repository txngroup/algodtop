// Number, unit and time formatting. Byte sizes use binary prefixes (KiB,
// MiB), and rates and counts use SI prefixes (k, M), as the terminal UI does.

const SI = [
  [1e12, "T"], [1e9, "G"], [1e6, "M"], [1e3, "k"],
];
const BIN = [
  [1024 ** 4, "Ti"], [1024 ** 3, "Gi"], [1024 ** 2, "Mi"], [1024, "Ki"],
];

export function isNum(v) {
  return typeof v === "number" && Number.isFinite(v);
}

function trim(n, digits) {
  let s = n.toFixed(digits);
  if (s.indexOf(".") >= 0) s = s.replace(/0+$/, "").replace(/\.$/, "");
  return s;
}

function sig(n) {
  const a = Math.abs(n);
  if (a >= 100) return trim(n, 0);
  if (a >= 10) return trim(n, 1);
  if (a >= 1) return trim(n, 2);
  if (a === 0) return "0";
  if (a >= 0.01) return trim(n, 3);
  return n.toPrecision(2);
}

// prefix picks the SI or binary prefix for a magnitude.
export function prefixFor(abs, binary) {
  const table = binary ? BIN : SI;
  for (const [div, p] of table) if (abs >= div) return [div, p];
  return [1, ""];
}

// unitInfo describes how a unit is shown.
export function unitInfo(unit) {
  switch (unit) {
    case "bytes": return { binary: true, suffix: "B" };
    case "B/s": return { binary: false, suffix: "B/s" };
    case "s": return { time: true };
    case "%": return { pct: true };
    case "": case undefined: case null: return { binary: false, suffix: "" };
    default: return { binary: false, suffix: unit.startsWith("/") ? unit : " " + unit, word: !unit.startsWith("/") };
  }
}

// fmtSeconds shows a duration value with one unit, for chart values.
export function fmtSeconds(v) {
  const a = Math.abs(v);
  if (a === 0) return "0 s";
  if (a < 1e-3) return sig(v * 1e6) + " us";
  if (a < 1) return sig(v * 1e3) + " ms";
  if (a < 120) return sig(v) + " s";
  if (a < 7200) return sig(v / 60) + " min";
  if (a < 172800) return sig(v / 3600) + " h";
  return sig(v / 86400) + " d";
}

// fmt formats one value with its unit.
export function fmt(v, unit) {
  if (!isNum(v)) return "-";
  const u = unitInfo(unit);
  if (u.time) return fmtSeconds(v);
  if (u.pct) return sig(v) + "%";
  const [div, p] = prefixFor(Math.abs(v), u.binary);
  if (u.word) return sig(v / div) + p + u.suffix;
  let suffix = u.suffix;
  const sep = p || (u.suffix && !u.suffix.startsWith(" ") && !u.suffix.startsWith("/")) ? " " : "";
  return sig(v / div) + sep + p + suffix;
}

// axisFormat returns a tick formatter that uses one prefix for all ticks.
export function axisFormat(unit, maxAbs, step) {
  const u = unitInfo(unit);
  if (u.time) {
    let div = 1, name = "s";
    if (maxAbs < 1) { div = 1e-3; name = "ms"; }
    else if (maxAbs >= 7200 * 24) { div = 86400; name = "d"; }
    else if (maxAbs >= 7200) { div = 3600; name = "h"; }
    else if (maxAbs >= 300) { div = 60; name = "min"; }
    const d = decimals(step / div);
    return (v) => (v / div).toFixed(d) + " " + name;
  }
  if (u.pct) {
    const d = decimals(step);
    return (v) => v.toFixed(d) + "%";
  }
  const [div, p] = prefixFor(maxAbs, u.binary);
  const d = decimals(step / div);
  // Word units (peers, txns) are named in the chart title; ticks stay short.
  if (u.word) return (v) => (v / div).toFixed(d) + p;
  let suffix = u.suffix;
  if (suffix.startsWith(" ")) suffix = p ? suffix.slice(1) : suffix;
  const sep = p || suffix.startsWith("B") ? " " : "";
  return (v) => (v / div).toFixed(d) + sep + p + suffix;
}

// decimals is the number of decimals that shows every tick of a step
// exactly: 2.5 needs one, 0.25 needs two.
function decimals(step) {
  if (!(step > 0)) return 0;
  for (let d = 0; d < 6; d++) {
    const k = step * Math.pow(10, d);
    if (Math.abs(k - Math.round(k)) < 1e-6 * Math.max(1, k)) return d;
  }
  return 6;
}

// fmtInt formats an integer with thousands separators.
export function fmtInt(v) {
  if (!isNum(v)) return "-";
  return Math.round(v).toLocaleString("en-US");
}

export function fmtPct(v) {
  return isNum(v) ? sig(v) + "%" : "-";
}

// fmtDur formats seconds as "3d 4h", "12m 3s" or "4.2s".
export function fmtDur(sec) {
  if (!isNum(sec)) return "-";
  const neg = sec < 0;
  let s = Math.abs(sec);
  let out;
  if (s < 10) out = s.toFixed(1) + "s";
  else if (s < 60) out = Math.round(s) + "s";
  else if (s < 3600) {
    const r = Math.round(s);
    out = Math.floor(r / 60) + "m " + (r % 60) + "s";
  }
  else if (s < 86400) out = Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
  else out = Math.floor(s / 86400) + "d " + Math.floor((s % 86400) / 3600) + "h";
  return (neg ? "-" : "") + out;
}

const pad = (n) => String(n).padStart(2, "0");
const MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

export function fmtClock(ms, secs) {
  const d = new Date(ms);
  return pad(d.getHours()) + ":" + pad(d.getMinutes()) + (secs ? ":" + pad(d.getSeconds()) : "");
}

export function fmtDay(ms) {
  const d = new Date(ms);
  return MON[d.getMonth()] + " " + d.getDate();
}

export function fmtMonth(ms) {
  const d = new Date(ms);
  return MON[d.getMonth()] + (d.getMonth() === 0 ? " " + d.getFullYear() : "");
}

// fmtDateTime is the full local time, "2026-11-03 13:48:02".
export function fmtDateTime(ms) {
  if (!isNum(ms) || ms <= 0) return "-";
  const d = new Date(ms);
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
}

// fmtShortTime shows a time with as much date as needed.
export function fmtShortTime(ms) {
  if (!isNum(ms) || ms <= 0) return "-";
  const now = new Date();
  const d = new Date(ms);
  if (d.toDateString() === now.toDateString()) return fmtClock(ms, true);
  if (d.getFullYear() === now.getFullYear()) return fmtDay(ms) + " " + fmtClock(ms, true);
  return fmtDateTime(ms);
}

// fmtAgo is "3m 2s ago".
export function fmtAgo(ms) {
  if (!isNum(ms) || ms <= 0) return "-";
  return fmtDur((Date.now() - ms) / 1000) + " ago";
}

// toLocalInput formats ms for an <input type="datetime-local">.
export function toLocalInput(ms) {
  const d = new Date(ms);
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + "T" + pad(d.getHours()) + ":" + pad(d.getMinutes());
}

// fmtDelta shows a signed change with its unit.
export function fmtDelta(d, unit) {
  if (!isNum(d)) return "";
  if (d === 0) return "no change";
  const sign = d > 0 ? "+" : "-";
  return sign + fmt(Math.abs(d), unit);
}

// ns converts API nanoseconds to milliseconds.
export const ns = (v) => (isNum(v) && v > 0 ? v / 1e6 : 0);

export function shortAddr(a) {
  if (!a) return "";
  return a.length > 14 ? a.slice(0, 6) + "..." + a.slice(-4) : a;
}
