// Fixed colour meanings. Inbound is cyan and outbound is orange, with
// WebSocket the lighter step and P2P the darker one. Other series take the
// categorical slots in a fixed order, and anything past the sixth folds
// into "other".

export const IN = "var(--in)";
export const OUT = "var(--out)";
export const FLOW = {
  "in-ws": "var(--in-ws)",
  "in-p2p": "var(--in-p2p)",
  "out-ws": "var(--out-ws)",
  "out-p2p": "var(--out-p2p)",
};
export const PAL = ["var(--s1)", "var(--s2)", "var(--s3)", "var(--s4)", "var(--s5)", "var(--s6)"];
export const OTHER = "var(--other)";

export function dirColor(dir, net) {
  const d = dir === "in" || dir === "inbound" || dir === "rx" ? "in" : "out";
  if (net === "ws" || net === "p2p") return FLOW[d + "-" + net];
  return d === "in" ? IN : OUT;
}

export const slot = (i) => (i < PAL.length ? PAL[i] : OTHER);
