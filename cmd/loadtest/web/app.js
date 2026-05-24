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
    // eslint-disable-next-line no-console
    console.log("snapshot:", (frame.key_stats || []).length, "keys,", (frame.logs || []).length, "logs");
  }
  function onStats(frame) {
    // eslint-disable-next-line no-console
    console.log("stats:", (frame.key_stats || []).length, "keys");
  }
  function onLogAppend(frame) {
    // eslint-disable-next-line no-console
    console.log("log_append:", (frame.logs || []).length, "new logs");
  }
  function onReset(frame) {
    // eslint-disable-next-line no-console
    console.log("reset:", frame.scope);
  }

  // ---------- kick off ----------------------------------------------------

  // Expose a couple of hooks for the dev console / future pieces.
  window.SHORTLINK_DEBUG = {
    cfg: cfg,
    socket: function () { return ws; },
  };

  connect();
})();
