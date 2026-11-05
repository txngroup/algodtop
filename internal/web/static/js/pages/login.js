// Login form, shown when the listener has a password and this browser has
// no session.

import { h } from "../dom.js";
import { login } from "../api.js";

export function mount(root, app, done) {
  const pw = h("input", { type: "password", autocomplete: "current-password", "aria-label": "Password", required: true });
  const msg = h("div", { class: "error", role: "alert" });
  const btn = h("button", { type: "submit", class: "primary" }, "Log in");
  const form = h("form", { class: "login", on: {
    submit: async (e) => {
      e.preventDefault();
      btn.disabled = true;
      msg.textContent = "";
      try {
        const s = await login(pw.value);
        if (s.role === "none") throw new Error("login did not start a session");
        done();
      } catch (err) {
        msg.textContent = err.message;
        btn.disabled = false;
        pw.select();
      }
    },
  } },
  h("h1", null, "algod.top"),
  h("p", { class: "muted" }, "This daemon needs a password. The read-only password shows status and charts; the full password also allows annotations, silences and settings."),
  h("label", null, "Password", h("br"), pw),
  btn, msg);
  root.replaceChildren(h("div", { id: "login" }, form));
  pw.focus();
  return { destroy() {} };
}
