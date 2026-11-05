// API access. Reads go through a small queue so chart fetches never take
// every browser connection: the SSE stream needs one of the six.

export const session = { role: "none", csrf: "", auth_required: false };

let onAuthLost = () => {};
export function setAuthLostHandler(fn) {
  onAuthLost = fn;
}

export class APIError extends Error {
  constructor(status, msg) {
    super(msg);
    this.status = status;
  }
}

const MAX_INFLIGHT = 4;
let inflight = 0;
const waiting = [];

function acquire() {
  if (inflight < MAX_INFLIGHT) {
    inflight++;
    return Promise.resolve();
  }
  return new Promise((res) => waiting.push(res));
}

function release() {
  const next = waiting.shift();
  if (next) next();
  else inflight--;
}

// qs builds a query string. Array values repeat the key.
export function qs(params) {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v === undefined || v === null || v === "") continue;
    if (Array.isArray(v)) v.forEach((x) => u.append(k, x));
    else u.append(k, String(v));
  }
  const s = u.toString();
  return s ? "?" + s : "";
}

async function parse(res) {
  const text = await res.text();
  let body = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch (e) {
    body = null;
  }
  if (!res.ok) {
    const msg = (body && body.error) || text || res.statusText;
    if (res.status === 401) onAuthLost();
    throw new APIError(res.status, msg);
  }
  return body;
}

// get fetches JSON. signal is an optional AbortSignal.
export async function get(path, params, signal) {
  await acquire();
  try {
    const res = await fetch(path + qs(params), { credentials: "same-origin", signal });
    return await parse(res);
  } finally {
    release();
  }
}

// send makes a state-changing request with the CSRF token.
export async function send(method, path, body, params) {
  const headers = { "Content-Type": "application/json" };
  if (session.csrf) headers["X-CSRF-Token"] = session.csrf;
  const res = await fetch(path + qs(params), {
    method,
    credentials: "same-origin",
    headers,
    body: body === undefined ? undefined : typeof body === "string" ? body : JSON.stringify(body),
  });
  return parse(res);
}

export async function loadSession() {
  const res = await fetch("/api/session", { credentials: "same-origin" });
  const s = await parse(res);
  Object.assign(session, s || {});
  return session;
}

export async function login(password) {
  await send("POST", "/api/login", { password });
  return loadSession();
}

export async function logout() {
  await send("POST", "/api/logout");
  return loadSession();
}

export const canWrite = () => session.role === "write";
