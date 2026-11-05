// Largest-Triangle-Three-Buckets downsampling. It is the fallback for a
// full-resolution (raw tier) request that returns more points than the
// chart has pixels. Gaps (null values) split the series into runs, and
// each run is downsampled on its own so a gap never gets bridged.

function lttbRun(t, v, idx, threshold) {
  const n = idx.length;
  if (threshold >= n || threshold < 3) return idx;
  const out = [idx[0]];
  const every = (n - 2) / (threshold - 2);
  let a = 0;
  for (let i = 0; i < threshold - 2; i++) {
    const s0 = Math.floor((i + 1) * every) + 1;
    const s1 = Math.min(Math.floor((i + 2) * every) + 1, n);
    let ax = 0, ay = 0;
    for (let j = s0; j < s1; j++) {
      ax += t[idx[j]];
      ay += v[idx[j]];
    }
    const len = s1 - s0 || 1;
    ax /= len;
    ay /= len;
    const r0 = Math.floor(i * every) + 1;
    const r1 = Math.floor((i + 1) * every) + 1;
    const px = t[idx[a]], py = v[idx[a]];
    let best = -1, pick = r0;
    for (let j = r0; j < r1; j++) {
      const area = Math.abs((px - ax) * (v[idx[j]] - py) - (px - t[idx[j]]) * (ay - py));
      if (area > best) {
        best = area;
        pick = j;
      }
    }
    out.push(idx[pick]);
    a = pick;
  }
  out.push(idx[n - 1]);
  return out;
}

// lttb returns new {t, v} arrays with about threshold points. null values
// are kept as gap markers between runs.
export function lttb(t, v, threshold) {
  if (t.length <= threshold) return { t, v };
  const runs = [];
  let cur = [];
  let total = 0;
  for (let i = 0; i < t.length; i++) {
    if (v[i] === null || v[i] === undefined || Number.isNaN(v[i])) {
      if (cur.length) runs.push(cur);
      cur = [];
      continue;
    }
    cur.push(i);
    total++;
  }
  if (cur.length) runs.push(cur);
  const ot = [], ov = [];
  runs.forEach((run, k) => {
    const share = Math.max(3, Math.round((threshold * run.length) / Math.max(1, total)));
    const keep = lttbRun(t, v, run, share);
    if (k > 0) {
      ot.push(t[run[0]]);
      ov.push(null);
    }
    for (const i of keep) {
      ot.push(t[i]);
      ov.push(v[i]);
    }
  });
  return { t: ot, v: ov };
}
