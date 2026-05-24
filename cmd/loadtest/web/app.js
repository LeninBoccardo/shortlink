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
  }
  function onStats(frame) {
    renderKeyTable(frame.key_stats || []);
  }
  function onLogAppend(frame) {
    // eslint-disable-next-line no-console
    console.log("log_append:", (frame.logs || []).length, "new logs");
  }
  function onReset(frame) {
    // eslint-disable-next-line no-console
    console.log("reset:", frame.scope);
  }

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

  // ---------- kick off ----------------------------------------------------

  // Expose a couple of hooks for the dev console / future pieces.
  window.SHORTLINK_DEBUG = {
    cfg: cfg,
    socket: function () { return ws; },
  };

  connect();
})();
