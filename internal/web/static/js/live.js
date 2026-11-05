// One multiplexed SSE stream for every node the UI shows. The browser's
// EventSource reconnects on its own after the server's retry interval.

const listeners = { status: new Set(), event: new Set(), conn: new Set() };
let es = null;
let nodesKey = "";
export const latest = { nodes: [], status: null, time: 0, connected: false };

export function on(kind, fn) {
  listeners[kind].add(fn);
  return () => listeners[kind].delete(fn);
}

function emit(kind, data) {
  for (const fn of listeners[kind]) {
    try {
      fn(data);
    } catch (e) {
      console.warn("listener failed", e);
    }
  }
}

function setConn(v) {
  latest.connected = v;
  emit("conn", v);
}

// start opens the stream for the named nodes, or reopens it when the list
// changed. An empty list means every node.
export function start(nodes) {
  const key = (nodes || []).slice().sort().join(",");
  if (es && key === nodesKey) return;
  stop();
  nodesKey = key;
  es = new EventSource("/api/stream" + (key ? "?nodes=" + encodeURIComponent(key) : ""));
  es.addEventListener("open", () => setConn(true));
  es.addEventListener("error", () => setConn(false));
  es.addEventListener("status", (m) => {
    let d;
    try {
      d = JSON.parse(m.data);
    } catch (e) {
      return;
    }
    latest.nodes = d.nodes || [];
    latest.status = d.status || null;
    latest.time = d.time || 0;
    setConn(true);
    emit("status", d);
  });
  es.addEventListener("event", (m) => {
    try {
      emit("event", JSON.parse(m.data));
    } catch (e) {
      /* ignore a bad message */
    }
  });
}

export function stop() {
  if (es) es.close();
  es = null;
  setConn(false);
}
