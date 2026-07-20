/* chronicle playground — vanilla JS front end over the WebAssembly engine.
 *
 * The engine exposes a global `chronicle` object (see cmd/chronicle-wasm).
 * Every call takes and returns plain JS objects; a returned {error, code}
 * means the engine rejected the call. There is no network and no framework:
 * this file boots the wasm, keeps one entity in focus, and draws the
 * bitemporal grid that is the whole point of the demo.
 */

(function () {
  "use strict";

  const SVGNS = "http://www.w3.org/2000/svg";

  // Categorical palette for colouring boxes by value. Chosen to stay legible
  // on both light and dark grid backgrounds.
  const PALETTE = [
    "#4e79a7", "#f28e2b", "#59a14f", "#e15759", "#b07aa1",
    "#76b7b2", "#edc948", "#ff9da7", "#9c755f", "#8cd17d",
  ];
  const INTENT_COLORS = {
    assert: "#4e79a7",
    correction: "#e15759",
    remainder: "#9aa4b2",
  };

  // Grid geometry (SVG user units; CSS scales the whole thing responsively).
  const W = 780, H = 460;
  const M = { left: 66, right: 30, top: 24, bottom: 54 };
  const x0 = M.left, x1 = W - M.right, plotW = x1 - x0;
  const y0 = M.top, y1 = H - M.bottom, plotH = y1 - y0;

  let C = null; // the chronicle engine

  const state = {
    kind: "employee",
    entity: "alice",
    records: [],
    colorMode: "value",
    validAt: null, // ISO string, source of truth for the crosshair
    txAt: null,
    selectedId: null,
    clockMs: Date.UTC(2024, 0, 1),
    geom: null,
    valueColors: new Map(),
  };

  // ---- time helpers --------------------------------------------------------

  function ms(s) { return s ? Date.parse(s) : null; }
  function iso(m) { return new Date(m).toISOString(); }

  function fmtTick(m) {
    const d = new Date(m);
    const mon = d.toLocaleDateString("en-US", { month: "short", timeZone: "UTC" });
    const day = d.getUTCDate();
    const yr = String(d.getUTCFullYear()).slice(2);
    return `${mon} ${day} ’${yr}`;
  }
  function fmtInstant(m) {
    return new Date(m).toLocaleString("en-US", {
      year: "numeric", month: "short", day: "numeric",
      hour: "2-digit", minute: "2-digit", timeZone: "UTC",
    }) + " UTC";
  }
  function fmtInterval(from, to, openLabel) {
    const a = from ? fmtTick(ms(from)) : "−∞";
    const b = to ? fmtTick(ms(to)) : (openLabel || "∞");
    return `[${a}, ${b})`;
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => (
      { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
    ));
  }

  // ---- small DOM/SVG builders ---------------------------------------------

  function svg(tag, attrs) {
    const e = document.createElementNS(SVGNS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }
  function $(id) { return document.getElementById(id); }

  // ---- value colouring -----------------------------------------------------

  function valueKey(r) { return JSON.stringify(r.data); }

  // rebuildValueColors assigns a stable colour to each distinct value, in the
  // order values first appear along transaction time.
  function rebuildValueColors(records) {
    const sorted = records.slice().sort((a, b) => ms(a.txFrom) - ms(b.txFrom));
    const map = new Map();
    let i = 0;
    for (const r of sorted) {
      const k = valueKey(r);
      if (!map.has(k)) { map.set(k, PALETTE[i % PALETTE.length]); i++; }
    }
    state.valueColors = map;
  }

  function fillFor(r) {
    if (state.colorMode === "intent") {
      return INTENT_COLORS[r.intent] || "#888";
    }
    return state.valueColors.get(valueKey(r)) || "#888";
  }

  // boxLabel is a compact rendering of a record's value for inside a box.
  function boxLabel(r) {
    const d = r.data;
    if (d === null || d === undefined) return "∅";
    if (typeof d === "object" && !Array.isArray(d)) {
      const keys = Object.keys(d);
      if (keys.length === 0) return "{}";
      const k = keys[0];
      let v = d[k];
      if (typeof v === "object") v = JSON.stringify(v);
      let s = `${k}:${v}`;
      return s.length > 16 ? s.slice(0, 15) + "…" : s;
    }
    return String(d);
  }

  function valueSummary(data) {
    if (data === null) return "null (tombstone)";
    if (typeof data === "object") {
      const parts = Object.entries(data).map(([k, v]) =>
        `${k}: ${typeof v === "object" ? JSON.stringify(v) : v}`);
      return parts.join(", ");
    }
    return String(data);
  }

  // ---- geometry ------------------------------------------------------------

  function computeGeom(records) {
    const vEdges = new Set(), tEdges = new Set();
    for (const r of records) {
      const vf = ms(r.validFrom), vt = ms(r.validTo);
      const tf = ms(r.txFrom), tt = ms(r.txTo);
      if (vf !== null) vEdges.add(vf);
      if (vt !== null) vEdges.add(vt);
      if (tf !== null) tEdges.add(tf);
      if (tt !== null) tEdges.add(tt);
    }
    // The clock anchors "now" at the top of the transaction axis.
    let nowMs = state.clockMs;
    for (const m of tEdges) nowMs = Math.max(nowMs, m);
    tEdges.add(nowMs);

    const vArr = [...vEdges].sort((a, b) => a - b);
    const tArr = [...tEdges].sort((a, b) => a - b);

    const DAY = 86400000;
    let vMin = vArr[0], vMax = vArr[vArr.length - 1];
    if (vMin === undefined) { vMin = Date.UTC(2024, 0, 1); vMax = Date.UTC(2024, 11, 31); }
    let vSpan = vMax - vMin; if (vSpan <= 0) vSpan = 90 * DAY;
    const vPad = vSpan * 0.14;

    let tMin = tArr[0], tMax = tArr[tArr.length - 1];
    if (tMin === undefined) { tMin = Date.UTC(2024, 0, 1); tMax = Date.UTC(2024, 6, 1); }
    let tSpan = tMax - tMin; if (tSpan <= 0) tSpan = 30 * DAY;
    const tPad = tSpan * 0.16;

    return {
      domVMin: vMin - vPad, domVMax: vMax + vPad,
      domTMin: tMin - tPad, domTMax: tMax + tPad,
      vEdges: vArr, tEdges: tArr, nowMs,
    };
  }

  function xOf(g, m) { return x0 + ((m - g.domVMin) / (g.domVMax - g.domVMin)) * plotW; }
  function yOf(g, m) { return y1 - ((m - g.domTMin) / (g.domTMax - g.domTMin)) * plotH; }
  function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }

  // Inverse maps for click/slider → instant.
  function validAtFromX(g, px) {
    const f = clamp((px - x0) / plotW, 0, 1);
    return g.domVMin + f * (g.domVMax - g.domVMin);
  }
  function txAtFromY(g, py) {
    const f = clamp((y1 - py) / plotH, 0, 1);
    return g.domTMin + f * (g.domTMax - g.domTMin);
  }

  // ---- the grid ------------------------------------------------------------

  function renderGrid() {
    const root = $("grid");
    root.setAttribute("viewBox", `0 0 ${W} ${H}`);
    root.setAttribute("preserveAspectRatio", "xMidYMid meet");
    while (root.firstChild) root.removeChild(root.firstChild);

    const records = state.records;
    const g = computeGeom(records);
    state.geom = g;

    // background + frame
    root.appendChild(svg("rect", { x: x0, y: y0, width: plotW, height: plotH, fill: "transparent", class: "frame" }));

    // valid-axis (x) gridlines + labels
    for (const m of g.vEdges) {
      const x = xOf(g, m);
      if (x < x0 - 1 || x > x1 + 1) continue;
      root.appendChild(svg("line", { x1: x, y1: y0, x2: x, y2: y1, class: "tick-line" }));
      const t = svg("text", { x: x, y: y1 + 16, "text-anchor": "middle", "font-size": 10.5 });
      t.textContent = fmtTick(m);
      root.appendChild(t);
    }
    // transaction-axis (y) gridlines + labels
    for (const m of g.tEdges) {
      const y = yOf(g, m);
      if (y < y0 - 1 || y > y1 + 1) continue;
      root.appendChild(svg("line", { x1: x0, y1: y, x2: x1, y2: y, class: "tick-line" }));
      const t = svg("text", { x: x0 - 6, y: y + 3.5, "text-anchor": "end", "font-size": 10.5 });
      t.textContent = fmtTick(m);
      root.appendChild(t);
    }

    // "now" marker at the top band
    const nowY = yOf(g, g.nowMs);
    const nowText = svg("text", { x: x1, y: nowY - 4, "text-anchor": "end", "font-size": 10, class: "now-label" });
    nowText.textContent = "now ↑ (current belief)";
    root.appendChild(nowText);

    // record boxes
    for (const r of records) {
      const left = r.validFrom ? xOf(g, ms(r.validFrom)) : x0;
      const right = r.validTo ? xOf(g, ms(r.validTo)) : x1;
      const top = r.txTo ? yOf(g, ms(r.txTo)) : y0;
      const bottom = yOf(g, ms(r.txFrom));
      const w = Math.max(1, right - left);
      const h = Math.max(1, bottom - top);

      const rect = svg("rect", {
        x: left, y: top, width: w, height: h,
        rx: 3, ry: 3,
        fill: fillFor(r),
        "fill-opacity": r.current ? 0.92 : 0.5,
        stroke: "var(--panel)",
        "stroke-width": 1,
        class: "rec" + (r.id === state.selectedId ? " selected" : ""),
      });
      rect.dataset.id = r.id;
      rect.addEventListener("click", (ev) => { ev.stopPropagation(); selectRecord(r); });
      const title = svg("title");
      title.textContent =
        `${valueSummary(r.data)}\nvalid ${fmtInterval(r.validFrom, r.validTo)}\n` +
        `tx ${fmtInterval(r.txFrom, r.txTo, "now")}\n${r.intent} · ${r.actor.id}` +
        (r.reason ? `\n“${r.reason}”` : "");
      rect.appendChild(title);
      root.appendChild(rect);

      // label if the box is large enough
      if (w > 42 && h > 16) {
        const label = svg("text", {
          x: left + w / 2, y: top + h / 2 + 3.5,
          "text-anchor": "middle", "font-size": 11, class: "rec-label",
        });
        label.textContent = boxLabel(r);
        root.appendChild(label);
      }
    }

    // axis titles
    const xt = svg("text", { x: x0 + plotW / 2, y: H - 6, "text-anchor": "middle", "font-size": 12, class: "axis-title" });
    xt.textContent = "valid time  (when the fact was true)  →";
    root.appendChild(xt);
    const yt = svg("text", {
      x: 14, y: y0 + plotH / 2, "text-anchor": "middle", "font-size": 12, class: "axis-title",
      transform: `rotate(-90 14 ${y0 + plotH / 2})`,
    });
    yt.textContent = "transaction time  (when we learned it)  ↑";
    root.appendChild(yt);

    // crosshair (as-of point)
    if (state.validAt && state.txAt) {
      const cx = clamp(xOf(g, ms(state.validAt)), x0, x1);
      const cy = clamp(yOf(g, ms(state.txAt)), y0, y1);
      root.appendChild(svg("line", { x1: cx, y1: y0, x2: cx, y2: y1, class: "crosshair" }));
      root.appendChild(svg("line", { x1: x0, y1: cy, x2: x1, y2: cy, class: "crosshair" }));
      root.appendChild(svg("circle", { cx: cx, cy: cy, r: 4.5, class: "crosshair-dot" }));
    }

    // click surface for as-of resolution
    const surface = svg("rect", { x: x0, y: y0, width: plotW, height: plotH, fill: "transparent" });
    surface.style.cursor = "crosshair";
    surface.addEventListener("click", (ev) => {
      const pt = svgPoint(root, ev);
      state.validAt = iso(validAtFromX(g, pt.x));
      state.txAt = iso(txAtFromY(g, pt.y));
      updateAsof();
    });
    root.appendChild(surface);

    renderLegend();
  }

  function svgPoint(root, ev) {
    const rect = root.getBoundingClientRect();
    const sx = W / rect.width, sy = H / rect.height;
    return { x: (ev.clientX - rect.left) * sx, y: (ev.clientY - rect.top) * sy };
  }

  function renderLegend() {
    const box = $("legend");
    box.innerHTML = "";
    const items = [];
    if (state.colorMode === "intent") {
      const seen = new Set(state.records.map((r) => r.intent));
      for (const it of ["assert", "correction", "remainder"]) {
        if (seen.has(it)) items.push([INTENT_COLORS[it], it]);
      }
    } else {
      for (const [key, color] of state.valueColors) {
        let label;
        try { label = valueSummary(JSON.parse(key)); } catch { label = key; }
        if (label.length > 34) label = label.slice(0, 33) + "…";
        items.push([color, label]);
      }
    }
    for (const [color, label] of items) {
      const span = document.createElement("span");
      span.className = "item";
      const sw = document.createElement("span");
      sw.className = "swatch";
      sw.style.background = color;
      span.appendChild(sw);
      span.appendChild(document.createTextNode(label));
      box.appendChild(span);
    }
    const hint = document.createElement("span");
    hint.className = "item";
    hint.style.color = "var(--ink-faint)";
    hint.textContent = "· solid = current belief, faded = superseded";
    box.appendChild(hint);
  }

  // ---- as-of explorer ------------------------------------------------------

  function selectRecord(r) {
    // Drop the crosshair into the middle of the clicked box.
    const g = state.geom;
    const vf = ms(r.validFrom) ?? g.domVMin;
    const vt = ms(r.validTo) ?? g.domVMax;
    const tf = ms(r.txFrom);
    const tt = ms(r.txTo) ?? g.domTMax;
    state.validAt = iso((vf + vt) / 2);
    state.txAt = iso((tf + tt) / 2);
    updateAsof();
  }

  function updateAsof() {
    const g = state.geom;
    if (!g) return;
    // sync sliders to the current instants
    $("validat-slider").value = String(Math.round(((ms(state.validAt) - g.domVMin) / (g.domVMax - g.domVMin)) * 1000));
    $("txat-slider").value = String(Math.round(((ms(state.txAt) - g.domTMin) / (g.domTMax - g.domTMin)) * 1000));
    $("validat-read").textContent = fmtInstant(ms(state.validAt));
    $("txat-read").textContent = fmtInstant(ms(state.txAt));

    const res = C.get({ kind: state.kind, entity: state.entity, validAt: state.validAt, txAt: state.txAt });
    const out = $("asof-result");
    if (res.error) {
      state.selectedId = null;
      out.innerHTML = `<span style="color:var(--danger)">${escapeHtml(res.error)}</span>`;
    } else if (!res.found) {
      state.selectedId = null;
      out.innerHTML =
        `<strong>No record here.</strong> At this (valid, transaction) point the entity had ` +
        `no recorded state — a genuine gap, not a zero.`;
    } else {
      const r = res.record;
      state.selectedId = r.id;
      out.innerHTML =
        `<strong>In force:</strong> <span style="font-family:var(--mono)">${escapeHtml(valueSummary(r.data))}</span> ` +
        `<span class="badge ${r.intent}">${r.intent}</span><br>` +
        `<span class="fh-meta">valid ${escapeHtml(fmtInterval(r.validFrom, r.validTo))} · ` +
        `tx ${escapeHtml(fmtInterval(r.txFrom, r.txTo, "now"))} · by ${escapeHtml(r.actor.id)}` +
        (r.reason ? ` · “${escapeHtml(r.reason)}”` : "") + `</span>`;
    }
    renderGrid();
  }

  function onSlider() {
    const g = state.geom;
    if (!g) return;
    const vv = Number($("validat-slider").value) / 1000;
    const tv = Number($("txat-slider").value) / 1000;
    state.validAt = iso(g.domVMin + vv * (g.domVMax - g.domVMin));
    state.txAt = iso(g.domTMin + tv * (g.domTMax - g.domTMin));
    updateAsof();
  }

  // ---- field history -------------------------------------------------------

  function fmtFieldValue(fv) {
    if (!fv.present) return `<span class="absent">∅ absent</span>`;
    if (fv.value === null || fv.value === undefined) return `null`;
    return escapeHtml(JSON.stringify(fv.value));
  }

  function renderFieldHistory() {
    const box = $("fieldhistory");
    const path = $("fh-path").value.trim();
    const validAt = $("fh-validat").value ? $("fh-validat").value + "T00:00:00Z" : "";
    box.innerHTML = "";
    if (!path) { box.innerHTML = `<p class="fh-empty">Enter a JSON Pointer, e.g. /salary</p>`; return; }

    const res = C.fieldHistory({ kind: state.kind, entity: state.entity, path, validAt });
    if (res.error) {
      box.innerHTML = `<p class="fh-empty" style="color:var(--danger)">${escapeHtml(res.error)}</p>`;
      return;
    }
    const revs = res.revisions || [];
    if (revs.length === 0) {
      box.innerHTML = `<p class="fh-empty">No changes to <code>${escapeHtml(path)}</code> at this valid instant.</p>`;
      return;
    }
    for (const rev of revs) {
      const row = document.createElement("div");
      row.className = "fh-row";
      row.innerHTML =
        `<div class="fh-transition">${fmtFieldValue(rev.from)}` +
        `<span class="arrow">→</span>${fmtFieldValue(rev.to)}</div>` +
        `<div class="fh-meta">${escapeHtml(fmtInstant(ms(rev.txAt)))} · ` +
        `${escapeHtml(rev.actor.id)} <span class="badge ${rev.intent}">${rev.intent}</span>` +
        (rev.reason ? ` · “${escapeHtml(rev.reason)}”` : "") + `</div>`;
      box.appendChild(row);
    }
  }

  // ---- records table (Query) ----------------------------------------------

  function renderRecordsTable() {
    const box = $("records-table");
    const q = { kind: state.kind, entity: state.entity, descending: true };
    if ($("q-current").checked) q.currentOnly = true;
    const intent = $("q-intent").value;
    if (intent) q.intent = intent;

    const res = C.query(q);
    box.innerHTML = "";
    if (res.error) { box.innerHTML = `<p class="fh-empty">${escapeHtml(res.error)}</p>`; return; }
    const recs = res.records || [];
    if (recs.length === 0) { box.innerHTML = `<p class="fh-empty">No records.</p>`; return; }

    const table = document.createElement("table");
    table.className = "recs";
    table.innerHTML =
      `<thead><tr><th>value</th><th>valid</th><th>transaction</th><th>actor</th><th>intent</th></tr></thead>`;
    const tb = document.createElement("tbody");
    for (const r of recs) {
      const tr = document.createElement("tr");
      tr.className = (r.current ? "" : "superseded") + (r.id === state.selectedId ? " hl" : "");
      tr.innerHTML =
        `<td class="val">${escapeHtml(valueSummary(r.data))}</td>` +
        `<td>${escapeHtml(fmtInterval(r.validFrom, r.validTo))}</td>` +
        `<td>${escapeHtml(fmtInterval(r.txFrom, r.txTo, "now"))}</td>` +
        `<td>${escapeHtml(r.actor.id)}</td>` +
        `<td><span class="badge ${r.intent}">${r.intent}</span></td>`;
      tr.addEventListener("click", () => selectRecord(r));
      tb.appendChild(tr);
    }
    table.appendChild(tb);
    box.appendChild(table);
  }

  // ---- entity selector -----------------------------------------------------

  function refreshEntities() {
    const res = C.query({});
    const sel = $("entity-select");
    const seen = new Map();
    for (const r of (res.records || [])) {
      const key = r.kind + "::" + r.entity;
      if (!seen.has(key)) seen.set(key, { kind: r.kind, entity: r.entity });
    }
    // Always include the currently focused entity even if it has no records yet.
    const curKey = state.kind + "::" + state.entity;
    if (!seen.has(curKey)) seen.set(curKey, { kind: state.kind, entity: state.entity });

    sel.innerHTML = "";
    for (const [key, ke] of seen) {
      const opt = document.createElement("option");
      opt.value = key;
      opt.textContent = `${ke.kind} / ${ke.entity}`;
      if (key === curKey) opt.selected = true;
      sel.appendChild(opt);
    }
  }

  // ---- clock ---------------------------------------------------------------

  function msToLocalInput(m) {
    // datetime-local wants "YYYY-MM-DDTHH:MM:SS"; we treat the value as UTC.
    return new Date(m).toISOString().slice(0, 19);
  }
  function setClockTo(m) {
    const res = C.setClock(iso(m));
    if (!res.error) state.clockMs = m;
    $("clock-input").value = msToLocalInput(state.clockMs);
  }

  // ---- refresh -------------------------------------------------------------

  function refresh() {
    const res = C.history({ kind: state.kind, entity: state.entity });
    state.records = res.records || [];
    rebuildValueColors(state.records);

    // default crosshair if unset: current belief (top) at the mid valid point
    if (!state.validAt || !state.txAt) {
      const g = computeGeom(state.records);
      state.validAt = iso((g.domVMin + g.domVMax) / 2);
      state.txAt = iso(g.nowMs);
    }
    renderGrid();
    renderFieldHistory();
    renderRecordsTable();
    updateAsof();
  }

  // ---- scenarios -----------------------------------------------------------

  function loadScenario(name, btn) {
    const res = C.loadScenario(name);
    if (res.error) { flashWrite(res.error, true); return; }
    state.kind = res.kind;
    state.entity = res.entity;
    state.validAt = res.focus.validAt || null;
    state.txAt = null; // recomputed to "now" in refresh

    // move the demo clock to the latest write so "now" sits at the top.
    // derive latest tx from the freshly written history
    const hist = C.history({ kind: res.kind, entity: res.entity }).records || [];
    let latest = state.clockMs;
    for (const r of hist) latest = Math.max(latest, ms(r.txFrom));
    setClockTo(latest);

    // prefill the field-history + write controls for the story
    if (res.path) $("fh-path").value = res.path;
    if (res.focus.validAt) $("fh-validat").value = res.focus.validAt.slice(0, 10);
    $("w-kind").value = res.kind;
    $("w-entity").value = res.entity;

    // note + active button
    const note = $("scenario-note");
    note.textContent = res.note || "";
    note.hidden = !res.note;
    document.querySelectorAll("#scenario-buttons .btn").forEach((b) => b.classList.remove("active"));
    if (btn) btn.classList.add("active");

    refreshEntities();
    refresh();
  }

  function buildScenarioButtons() {
    const wrap = $("scenario-buttons");
    wrap.innerHTML = "";
    const list = (C.scenarios || []);
    for (const sc of list) {
      const b = document.createElement("button");
      b.className = "btn";
      b.type = "button";
      b.textContent = sc.title;
      b.title = sc.blurb || "";
      b.addEventListener("click", () => loadScenario(sc.name, b));
      wrap.appendChild(b);
    }
  }

  // ---- write controls ------------------------------------------------------

  function flashWrite(msg, isErr) {
    const el = $("write-msg");
    el.textContent = msg;
    el.className = "write-msg " + (isErr ? "err" : "ok");
  }

  function doWrite(correct) {
    const kind = $("w-kind").value.trim();
    const entity = $("w-entity").value.trim();
    let data;
    try {
      data = JSON.parse($("w-data").value);
    } catch (e) {
      flashWrite("Data is not valid JSON: " + e.message, true);
      return;
    }
    const validFrom = $("w-validfrom").value ? $("w-validfrom").value + "T00:00:00Z" : "";
    const validTo = $("w-validto").value ? $("w-validto").value + "T00:00:00Z" : "";
    const actor = $("w-actor").value.trim();
    const reason = $("w-reason").value.trim();

    const fn = correct ? C.correct : C.put;
    const res = fn({ kind, entity, data, validFrom, validTo, actor, reason });
    if (res.error) { flashWrite(`${res.error} (${res.code})`, true); return; }

    flashWrite(
      `${correct ? "Corrected" : "Asserted"} at ${fmtInstant(ms(res.txAt))} · ` +
      `${res.superseded.length} superseded, ${res.written.length} written.`, false);

    // focus the entity we just wrote and re-read everything
    state.kind = kind;
    state.entity = entity;
    // keep the crosshair on the same valid point but move belief to "now"
    state.txAt = null;
    refreshEntities();
    refresh();
  }

  // ---- wiring --------------------------------------------------------------

  function wire() {
    $("reset-btn").addEventListener("click", () => {
      const res = C.reset();
      if (res.clock) state.clockMs = Date.parse(res.clock);
      state.kind = $("w-kind").value.trim() || "employee";
      state.entity = $("w-entity").value.trim() || "alice";
      state.validAt = null; state.txAt = null; state.selectedId = null;
      $("scenario-note").hidden = true;
      document.querySelectorAll("#scenario-buttons .btn").forEach((b) => b.classList.remove("active"));
      $("clock-input").value = msToLocalInput(state.clockMs);
      refreshEntities();
      refresh();
      flashWrite("Log reset — empty.", false);
    });

    $("entity-select").addEventListener("change", (e) => {
      const [kind, entity] = e.target.value.split("::");
      state.kind = kind; state.entity = entity;
      state.validAt = null; state.txAt = null; state.selectedId = null;
      $("w-kind").value = kind; $("w-entity").value = entity;
      refresh();
    });

    document.querySelectorAll(".colormode .chip").forEach((chip) => {
      chip.addEventListener("click", () => {
        state.colorMode = chip.dataset.mode;
        document.querySelectorAll(".colormode .chip").forEach((c) =>
          c.classList.toggle("active", c === chip));
        renderGrid();
      });
    });

    $("validat-slider").addEventListener("input", onSlider);
    $("txat-slider").addEventListener("input", onSlider);

    $("put-btn").addEventListener("click", () => doWrite(false));
    $("correct-btn").addEventListener("click", () => doWrite(true));

    $("clock-set").addEventListener("click", () => {
      const v = $("clock-input").value;
      if (!v) return;
      const m = Date.parse(v + "Z"); // interpret input as UTC
      if (!isNaN(m)) setClockTo(m);
    });

    $("fh-path").addEventListener("input", renderFieldHistory);
    $("fh-validat").addEventListener("input", renderFieldHistory);
    $("q-current").addEventListener("change", renderRecordsTable);
    $("q-intent").addEventListener("change", renderRecordsTable);
  }

  // ---- boot ----------------------------------------------------------------

  let started = false;
  function start() {
    if (started) return; // the ready callback can fire during go.run
    started = true;
    C = window.chronicle;
    // colour-mode default
    document.querySelector('.colormode .chip[data-mode="value"]').classList.add("active");
    $("clock-input").value = msToLocalInput(state.clockMs);
    buildScenarioButtons();
    wire();
    $("boot").classList.add("hidden");
    // open on the flagship story
    const flagshipBtn = document.querySelector("#scenario-buttons .btn");
    loadScenario("salary", flagshipBtn);
  }

  function boot() {
    if (!window.Go) {
      fail("wasm_exec.js failed to load.");
      return;
    }
    const go = new Go();
    const url = "chronicle.wasm";
    const ready = () => start();
    window.__chronicleOnReady = ready;

    const inst = (obj) => {
      go.run(obj.instance);
      if (window.__chronicleReady) ready(); // in case it resolved synchronously
    };
    if (WebAssembly.instantiateStreaming) {
      WebAssembly.instantiateStreaming(fetch(url), go.importObject)
        .then(inst).catch((e) => fallback(go, url, inst, e));
    } else {
      fallback(go, url, inst);
    }
  }

  function fallback(go, url, inst) {
    fetch(url).then((r) => r.arrayBuffer())
      .then((buf) => WebAssembly.instantiate(buf, go.importObject))
      .then(inst).catch((e) => fail("Could not load the wasm module: " + e));
  }

  function fail(msg) {
    const b = $("boot");
    b.textContent = msg;
    b.classList.add("error");
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
