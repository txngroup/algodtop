// Small DOM helpers. Text always goes in through textContent, never as
// HTML, because labels and messages come from algod and peers.

const SVGNS = "http://www.w3.org/2000/svg";

function apply(el, attrs) {
  if (!attrs) return;
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") el.setAttribute("class", v);
    else if (k === "style" && typeof v === "object") Object.assign(el.style, v);
    else if (k === "on") for (const [ev, fn] of Object.entries(v)) el.addEventListener(ev, fn);
    else if (k === "dataset") Object.assign(el.dataset, v);
    else if (k === "text") el.textContent = v;
    else if (k === "value" && "value" in el) el.value = v;
    else if (k === "checked" || k === "selected" || k === "disabled" || k === "open") el[k] = !!v;
    else if (v === true) el.setAttribute(k, "");
    else el.setAttribute(k, String(v));
  }
}

function append(el, children) {
  for (const c of children) {
    if (c === undefined || c === null || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.appendChild(c);
    else el.appendChild(document.createTextNode(String(c)));
  }
}

// h builds an HTML element: h("a", {href: "/"}, "text", child).
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  apply(el, attrs);
  append(el, children);
  return el;
}

// s builds an SVG element.
export function s(tag, attrs, ...children) {
  const el = document.createElementNS(SVGNS, tag);
  apply(el, attrs);
  append(el, children);
  return el;
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function $(sel, root) {
  return (root || document).querySelector(sel);
}

// tooltip shows the single floating tooltip near a client position.
const tip = () => document.getElementById("tooltip");

export function showTip(content, x, y) {
  const el = tip();
  if (!el) return;
  clear(el);
  append(el, [content]);
  el.hidden = false;
  const w = el.offsetWidth, ht = el.offsetHeight;
  let left = x + 14, top = y + 14;
  if (left + w > window.innerWidth - 8) left = Math.max(8, x - w - 14);
  if (top + ht > window.innerHeight - 8) top = Math.max(8, y - ht - 14);
  el.style.left = left + "px";
  el.style.top = top + "px";
}

export function hideTip() {
  const el = tip();
  if (el) el.hidden = true;
}

// debounce delays fn until calls stop for ms.
export function debounce(fn, ms) {
  let t = 0;
  return (...a) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...a), ms);
  };
}

// sortableTable renders rows into a table with clickable headers.
// cols: [{key, label, num, get(row) -> sort value, render(row) -> node|string, title}]
export function sortableTable(cols, rows, opts = {}) {
  let sortKey = opts.sort || null;
  let desc = opts.desc !== undefined ? opts.desc : true;
  const table = h("table", { class: "t" });
  const thead = h("thead");
  const tbody = h("tbody");
  table.append(thead, tbody);
  function draw() {
    clear(thead);
    const tr = h("tr");
    for (const c of cols) {
      const th = h("th", { class: (c.sort === false ? "" : "sortable ") + (c.num ? "num " : "") + (sortKey === c.key ? (desc ? "sorted-desc" : "sorted-asc") : ""), title: c.title || null }, c.label);
      if (c.sort !== false) {
        th.addEventListener("click", () => {
          if (sortKey === c.key) desc = !desc;
          else { sortKey = c.key; desc = !!c.num; }
          if (opts.onSort) opts.onSort(sortKey, desc);
          draw();
        });
      }
      tr.appendChild(th);
    }
    thead.appendChild(tr);
    clear(tbody);
    let list = rows.slice();
    if (sortKey) {
      const c = cols.find((x) => x.key === sortKey);
      if (c) {
        const get = c.get || ((r) => r[c.key]);
        list.sort((a, b) => {
          let va = get(a), vb = get(b);
          if (va === undefined || va === null || Number.isNaN(va)) va = c.num ? -Infinity : "";
          if (vb === undefined || vb === null || Number.isNaN(vb)) vb = c.num ? -Infinity : "";
          const r = typeof va === "number" && typeof vb === "number" ? va - vb : String(va).localeCompare(String(vb));
          return desc ? -r : r;
        });
      }
    }
    if (opts.limit) list = list.slice(0, opts.limit);
    if (!list.length) {
      tbody.appendChild(h("tr", null, h("td", { colspan: cols.length, class: "muted" }, opts.empty || "Nothing to show.")));
    }
    for (const r of list) {
      const row = h("tr", { class: opts.rowClass ? opts.rowClass(r) : null });
      if (opts.onRow) {
        row.classList.add("click");
        row.addEventListener("click", (e) => opts.onRow(r, e, row));
      }
      for (const c of cols) {
        const v = c.render ? c.render(r) : (c.get ? c.get(r) : r[c.key]);
        row.appendChild(h("td", { class: (c.num ? "num " : "") + (c.cls || "") }, v === undefined || v === null ? "" : v));
      }
      tbody.appendChild(row);
      if (opts.after) {
        const extra = opts.after(r);
        if (extra) tbody.appendChild(extra);
      }
    }
  }
  draw();
  table.redraw = (newRows) => {
    if (newRows) rows = newRows;
    draw();
  };
  return table;
}

// inlineBar is a table cell with a bar scaled to max and a value.
export function inlineBar(v, max, label, cls) {
  const pct = max > 0 ? Math.max(0, Math.min(100, (v / max) * 100)) : 0;
  return h("div", { class: "ibar" },
    h("div", { class: "track" }, h("div", { class: "fill " + (cls || ""), style: { width: pct.toFixed(1) + "%" } })),
    h("span", { class: "val" }, label));
}
