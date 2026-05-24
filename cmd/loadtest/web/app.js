// ShortLink showcase page — vanilla JS, no build step.
//
// Piece 2 (this commit): WebSocket client + auto-reconnect with exponential
// backoff + connection indicator. Frame router dispatches snapshot/stats/
// log_append/reset to handlers — pieces 3-5 implement those handlers.

(function () {
  "use strict";

  // ---------- config + DOM refs --------------------------------------------

  var cfg = window.SHORTLINK_CONFIG || {};
  var wsURL = cfg.observer_ws || "ws://localhost:9090/stream";

  var els = {
    indicator: document.getElementById("conn-indicator"),
    connText: document.getElementById("conn-text"),
    btnReset: document.getElementById("btn-reset"),
    btnClear: document.getElementById("btn-clear"),
    keyTbody: document.getElementById("key-tbody"),
    logList: document.getElementById("log-list"),
    filterSource: document.getElementById("filter-source"),
    filterLevel: document.getElementById("filter-level"),
    filterKey: document.getElementById("filter-key"),
  };

  // ---------- connection indicator -----------------------------------------

  function setConn(state, text) {
    if (!els.indicator) return;
    els.indicator.className = "conn conn-" + state;
    if (els.connText) els.connText.textContent = text;
    var live = state === "open";
    if (els.btnReset) els.btnReset.disabled = !live;
    if (els.btnClear) els.btnClear.disabled = !live;
  }

  // ---------- WebSocket with exponential backoff ---------------------------
  //
  // SPEC §11: "Connection indicator turns grey and shows 'Reconnecting…' if
  // the WS drops; auto-reconnects with exponential backoff. On reconnect the
  // server re-sends a `snapshot`."
  //
  // The server resends snapshot automatically — we just need to reopen the
  // socket and let the frame router handle it.

  var BACKOFF_INITIAL = 500;     // ms
  var BACKOFF_MAX     = 15000;   // ms — cap so we don't sit silent for ages
  var ws = null;
  var backoff = BACKOFF_INITIAL;
  var reconnectTimer = null;
  var manualClose = false;

  function connect() {
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    setConn("init", "Connecting…");
    try {
      ws = new WebSocket(wsURL);
    } catch (err) {
      // Bad URL or browser refused outright. Schedule a retry like any
      // other failure rather than throwing — keeps the auto-reconnect
      // semantics consistent.
      // eslint-disable-next-line no-console
      console.error("ws construct failed:", err);
      scheduleReconnect();
      return;
    }
    ws.onopen = function () {
      backoff = BACKOFF_INITIAL;
      setConn("open", "Live");
    };
    ws.onmessage = function (ev) {
      try {
        var frame = JSON.parse(ev.data);
        routeFrame(frame);
      } catch (err) {
        // eslint-disable-next-line no-console
        console.warn("malformed WS frame:", err, ev.data);
      }
    };
    ws.onerror = function () {
      // Error events come before close on most browsers — let onclose
      // schedule the retry so we don't double-schedule.
      setConn("reconnect", "Connection error");
    };
    ws.onclose = function () {
      ws = null;
      if (manualClose) return;
      scheduleReconnect();
    };
  }

  function scheduleReconnect() {
    var delay = backoff;
    backoff = Math.min(backoff * 2, BACKOFF_MAX);
    setConn("reconnect", "Reconnecting in " + Math.round(delay / 100) / 10 + "s…");
    reconnectTimer = setTimeout(connect, delay);
  }

  // Reconnect quickly when the tab regains visibility — browsers throttle
  // background timers, so an idle tab may sit on a long backoff.
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible" && !ws && !manualClose) {
      backoff = BACKOFF_INITIAL;
      connect();
    }
  });

  // ---------- frame router -------------------------------------------------
  //
  // Frame shapes are defined by the broadcaster (internal/observer/broadcaster.go):
  //   snapshot   { type, ts, key_stats[], logs[], system }
  //   stats      { type, ts, key_stats[], system }
  //   log_append { type, ts, logs[] }
  //   reset      { type, scope }
  //
  // Handlers below are stubs in this commit — table + log rendering land in
  // pieces 3-5. We at least exercise the parsing so the WS layer is testable
  // standalone.

  function routeFrame(frame) {
    switch (frame && frame.type) {
      case "snapshot":
        onSnapshot(frame);
        break;
      case "stats":
        onStats(frame);
        break;
      case "log_append":
        onLogAppend(frame);
        break;
      case "reset":
        onReset(frame);
        break;
      default:
        // eslint-disable-next-line no-console
        console.warn("unknown frame type:", frame);
    }
  }

  function onSnapshot(frame) {
    renderKeyTable(frame.key_stats || []);
    resetLogs(frame.logs || []);
  }
  function onStats(frame) {
    renderKeyTable(frame.key_stats || []);
  }
  function onLogAppend(frame) {
    appendLogs(frame.logs || []);
  }
  function onReset(frame) {
    // Server is telling every client to wipe the matching scope.
    if (frame.scope === "logs") {
      resetLogs([]);
    } else if (frame.scope === "stats") {
      renderKeyTable([]);
    }
  }

  // ---------- cmd buttons -------------------------------------------------

  function sendCmd(action) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: "cmd", action: action }));
  }
  if (els.btnClear) els.btnClear.addEventListener("click", function () { sendCmd("clear_logs"); });
  if (els.btnReset) els.btnReset.addEventListener("click", function () { sendCmd("reset_stats"); });

  // ---------- key-stats table ---------------------------------------------
  //
  // SPEC §11:
  //   - Table rows keyed by key_hash (NOT key_hint — two keys could share
  //     the same last-6).
  //   - Red highlight when limit_errors / total_reqs > 0.5.
  //   - Hint shown as "..abc123" (last 6 chars).
  //   - Unlimited tier rendered with a "∞" budget.
  //
  // We keep a per-hash <tr> cache so the broadcaster's 500ms tick re-uses
  // existing rows and only touches the cells whose text changed. Cheap, no
  // framework, no flicker.

  var rowByHash = Object.create(null);

  function renderKeyTable(keys) {
    if (!els.keyTbody) return;
    if (!keys.length) {
      // Clear cache so a future reset_stats can rebuild cleanly.
      rowByHash = Object.create(null);
      els.keyTbody.innerHTML = '<tr class="empty"><td colspan="8">No keys seen yet.</td></tr>';
      return;
    }
    // Drop the empty placeholder on first real render.
    if (els.keyTbody.firstElementChild && els.keyTbody.firstElementChild.classList.contains("empty")) {
      els.keyTbody.innerHTML = "";
    }
    // keys arrive already sorted by key_hash (observer/state.go Snapshot
    // sorts before shipping); render in that order so row position is stable.
    var seen = Object.create(null);
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i];
      seen[k.key_hash] = true;
      var tr = rowByHash[k.key_hash];
      if (!tr) {
        tr = buildKeyRow(k);
        rowByHash[k.key_hash] = tr;
        els.keyTbody.appendChild(tr);
      } else {
        updateKeyRow(tr, k);
      }
    }
    // Evict rows for keys the observer no longer reports (e.g. after a
    // reset_stats; or after the observer's idle-key eviction kicked in).
    Object.keys(rowByHash).forEach(function (hash) {
      if (!seen[hash]) {
        els.keyTbody.removeChild(rowByHash[hash]);
        delete rowByHash[hash];
      }
    });
  }

  function buildKeyRow(k) {
    var tr = document.createElement("tr");
    tr.dataset.keyHash = k.key_hash;
    // Build all eight cells once; updateKeyRow swaps textContent.
    ["key", "tier", "rl", "reqs", "wh", "lim", "err", "p99"].forEach(function (cls) {
      var td = document.createElement("td");
      td.className = (cls === "key" || cls === "tier" || cls === "rl")
        ? "col-" + cls
        : "col-num";
      tr.appendChild(td);
    });
    updateKeyRow(tr, k);
    return tr;
  }

  function updateKeyRow(tr, k) {
    var cells = tr.children;
    cells[0].textContent = "…" + (k.key_hint || "??????");
    cells[1].textContent = k.tier || "-";
    cells[2].textContent = formatRateLimit(k);
    cells[3].textContent = formatInt(k.total_reqs);
    cells[4].textContent = formatInt(k.webhooks);
    cells[5].textContent = formatInt(k.limit_errors);
    cells[6].textContent = formatInt(k.job_errors);
    cells[7].textContent = formatLatency(k.p99_latency_ms);

    // Red-highlight when majority of requests got rate-limited (SPEC §11).
    var over = k.total_reqs > 0 && k.limit_errors / k.total_reqs > 0.5;
    tr.classList.toggle("over-limit", over);
  }

  function formatRateLimit(k) {
    if (k.rate_limit === 0) return "∞";       // unlimited
    if (!k.rate_limit) return "-";
    return k.rate_limit + "/min";
  }
  function formatInt(n) { return (typeof n === "number") ? String(n) : "0"; }
  function formatLatency(ms) {
    if (!ms) return "—";
    return ms + " ms";
  }

  // ---------- log audit panel ---------------------------------------------
  //
  // SPEC §4.3 / §11:
  //   - newest-first ring buffer, max 500 entries on the browser side.
  //   - per-entry TTL badge ticking down (badge derived from expires_at,
  //     re-rendered every 1s by a single setInterval).
  //   - on snapshot: reset to the server-shipped logs (already newest-first).
  //   - on log_append: prepend new entries (server ships them newest-first
  //     too — the ones since this client's cursor).
  //   - browser-side prune drops entries past expires_at so even a stuck WS
  //     decays the on-screen list naturally.

  var LOG_RING_MAX = 500;
  var logs = [];                  // newest-first, max LOG_RING_MAX
  var logLiByID = Object.create(null);

  function resetLogs(initial) {
    logs = [];
    logLiByID = Object.create(null);
    if (els.logList) els.logList.innerHTML = "";
    appendLogs(initial);
  }

  function appendLogs(fresh) {
    if (!els.logList || !fresh || !fresh.length) {
      tickLogTTLs(); // still re-render the existing badges
      return;
    }
    // Drop the empty placeholder on first real append.
    if (els.logList.firstElementChild && els.logList.firstElementChild.classList.contains("empty")) {
      els.logList.innerHTML = "";
    }
    var now = Date.now();
    var newSources = false, newLevels = false, newHints = false;
    // Iterate the incoming batch oldest-first so prepending one at a time
    // ends with the newest at the top.
    for (var i = fresh.length - 1; i >= 0; i--) {
      var entry = fresh[i];
      if (!entry || !entry.id || logLiByID[entry.id]) continue; // dedup on reconnect snapshot overlap
      var li = buildLogRow(entry, now);
      applyFilterToRow(li);
      logLiByID[entry.id] = li;
      logs.unshift(entry);
      els.logList.insertBefore(li, els.logList.firstChild);
      if (entry.source && !filterOpts.sources[entry.source]) { filterOpts.sources[entry.source] = true; newSources = true; }
      if (entry.level && !filterOpts.levels[entry.level]) { filterOpts.levels[entry.level] = true; newLevels = true; }
      if (entry.api_key_hint && !filterOpts.hints[entry.api_key_hint]) { filterOpts.hints[entry.api_key_hint] = true; newHints = true; }
    }
    // Cap the ring.
    while (logs.length > LOG_RING_MAX) {
      var evicted = logs.pop();
      var oldLi = logLiByID[evicted.id];
      if (oldLi && oldLi.parentNode) oldLi.parentNode.removeChild(oldLi);
      delete logLiByID[evicted.id];
    }
    if (newSources) rebuildFilterSelect(els.filterSource, filterOpts.sources);
    if (newLevels)  rebuildFilterSelect(els.filterLevel, filterOpts.levels);
    if (newHints)   rebuildFilterSelect(els.filterKey, filterOpts.hints, function (h) { return "…" + h; });
    tickLogTTLs();
  }

  function buildLogRow(entry, now) {
    var li = document.createElement("li");
    li.dataset.id = entry.id;
    li.dataset.expires = entry.expires_at || "";
    li.dataset.source = entry.source || "";
    li.dataset.level = entry.level || "info";
    li.dataset.hint = entry.api_key_hint || "";

    var ts = entry.ts ? new Date(entry.ts) : new Date(now);
    li.appendChild(span("log-ts", ts.toLocaleTimeString()));
    li.appendChild(span("log-src", entry.source || "?"));
    li.appendChild(span("log-lvl log-lvl-" + (entry.level || "info"), (entry.level || "info")));
    li.appendChild(span("log-kind", entry.kind || ""));
    li.appendChild(span("log-hint", entry.api_key_hint ? "…" + entry.api_key_hint : ""));
    li.appendChild(span("log-msg", entry.message || ""));
    var ttl = span("log-ttl", computeTTLBadge(entry, now));
    li.appendChild(ttl);
    return li;
  }

  function span(cls, text) {
    var el = document.createElement("span");
    el.className = cls;
    el.textContent = text;
    return el;
  }

  function computeTTLBadge(entry, now) {
    if (!entry || !entry.expires_at) return "";
    var exp = Date.parse(entry.expires_at);
    if (isNaN(exp)) return "";
    var s = Math.round((exp - now) / 1000);
    if (s <= 0) return "expired";
    if (s < 60) return s + "s";
    if (s < 3600) return Math.round(s / 60) + "m";
    return Math.round(s / 3600) + "h";
  }

  // tickLogTTLs walks the currently-rendered list and refreshes each badge.
  // Also drops entries past their expires_at — server pruned them out of the
  // ring already, but the browser keeps its own copy and needs to mirror.
  function tickLogTTLs() {
    if (!els.logList) return;
    var now = Date.now();
    for (var i = logs.length - 1; i >= 0; i--) {
      var entry = logs[i];
      if (!entry.expires_at) continue;
      var exp = Date.parse(entry.expires_at);
      if (!isNaN(exp) && exp <= now) {
        var li = logLiByID[entry.id];
        if (li && li.parentNode) li.parentNode.removeChild(li);
        delete logLiByID[entry.id];
        logs.splice(i, 1);
        continue;
      }
      var li2 = logLiByID[entry.id];
      if (!li2) continue;
      var badge = li2.lastChild;
      if (!badge) continue;
      var text = computeTTLBadge(entry, now);
      if (badge.textContent !== text) badge.textContent = text;
      // Warn colour when <30s.
      var s = (exp - now) / 1000;
      badge.classList.toggle("expiring", s < 30);
    }
  }

  setInterval(tickLogTTLs, 1000);

  // ---------- filter controls ---------------------------------------------
  //
  // SPEC §11: "Log filter dropdown: by source (api / worker / loadtest), by
  // level (warn/error only), by API key hint." We populate the option list
  // incrementally as new sources / levels / hints arrive in events, and
  // apply the current filter to every <li> via a `filtered` class — no
  // re-render on filter change, just classList toggles.

  var filterOpts = {
    sources: Object.create(null),
    levels: Object.create(null),
    hints: Object.create(null),
  };

  function rebuildFilterSelect(sel, set, format) {
    if (!sel) return;
    var current = sel.value;
    var keys = Object.keys(set).sort();
    sel.innerHTML = '<option value="">all</option>' +
      keys.map(function (k) {
        var label = format ? format(k) : k;
        return '<option value="' + k + '">' + label + '</option>';
      }).join("");
    if (current && set[current]) sel.value = current;
  }

  function applyFilterToRow(li) {
    var src = els.filterSource ? els.filterSource.value : "";
    var lvl = els.filterLevel ? els.filterLevel.value : "";
    var hint = els.filterKey ? els.filterKey.value : "";
    var hide = (src && li.dataset.source !== src) ||
               (lvl && li.dataset.level !== lvl) ||
               (hint && li.dataset.hint !== hint);
    li.classList.toggle("filtered", hide);
  }

  function applyFilterToAll() {
    if (!els.logList) return;
    var rows = els.logList.children;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].dataset && rows[i].dataset.id) applyFilterToRow(rows[i]);
    }
  }

  ["filterSource", "filterLevel", "filterKey"].forEach(function (k) {
    if (els[k]) els[k].addEventListener("change", applyFilterToAll);
  });

  // ---------- kick off ----------------------------------------------------

  // Expose a couple of hooks for the dev console / future pieces.
  window.SHORTLINK_DEBUG = {
    cfg: cfg,
    socket: function () { return ws; },
  };

  // Point each slot's iframe at its provisioned Grafana dashboard. The uids
  // (jobs-error-rate, qr-queue-depth) match the dashboard JSON committed in
  // deploy/grafana/dashboards/. kiosk=tv hides the chrome; theme=dark blends
  // into the showcase. If grafana_url is missing we mark the slot no-grafana
  // and the CSS shows the fallback hint instead of a broken iframe.
  (function wireGrafanaSlots() {
    var base = cfg.grafana_url || "";
    var slots = document.querySelectorAll(".grafana-slot");
    for (var i = 0; i < slots.length; i++) {
      var slot = slots[i];
      var iframe = slot.querySelector(".grafana-frame");
      var fallback = slot.querySelector(".grafana-fallback");
      if (!base) {
        slot.classList.add("no-grafana");
        if (fallback) fallback.textContent = "Grafana not configured (--grafana flag).";
        continue;
      }
      slot.classList.remove("no-grafana");
      var uid = iframe ? iframe.getAttribute("data-uid") : null;
      if (iframe && uid) {
        iframe.src = base.replace(/\/+$/, "") +
          "/d/" + encodeURIComponent(uid) +
          "?kiosk=tv&theme=dark&refresh=5s&from=now-15m&to=now";
      }
    }
  })();

  connect();
})();
