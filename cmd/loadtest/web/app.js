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
  //   - Table rows keyed by key_hint. The server identifies keys internally
  //     by SHA-256 hash but does NOT broadcast the hash (it's the same value
  //     stored as the credential record — least-privilege). Hints are 6
  //     base62 chars; collisions across a portfolio's worth of keys are
  //     effectively impossible (~62^-6 per pair).
  //   - Red highlight when limit_errors / total_reqs > 0.5.
  //   - Hint shown as "..abc123" (last 6 chars).
  //   - Unlimited tier rendered with a "∞" budget.
  //
  // We keep a per-hint <tr> cache so the broadcaster's 500ms tick re-uses
  // existing rows and only touches the cells whose text changed. Cheap, no
  // framework, no flicker.

  var rowByHint = Object.create(null);

  function renderKeyTable(keys) {
    if (!els.keyTbody) return;
    if (!keys.length) {
      // Clear cache so a future reset_stats can rebuild cleanly.
      rowByHint = Object.create(null);
      els.keyTbody.innerHTML = '<tr class="empty"><td colspan="8">No keys seen yet.</td></tr>';
      return;
    }
    // Drop the empty placeholder on first real render.
    if (els.keyTbody.firstElementChild && els.keyTbody.firstElementChild.classList.contains("empty")) {
      els.keyTbody.innerHTML = "";
    }
    // keys arrive sorted server-side (observer/state.go Snapshot sorts by the
    // internal hash before shipping); render in that order so row position
    // is stable.
    var seen = Object.create(null);
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i];
      var hint = k.key_hint || "";
      seen[hint] = true;
      var tr = rowByHint[hint];
      if (!tr) {
        tr = buildKeyRow(k);
        rowByHint[hint] = tr;
        els.keyTbody.appendChild(tr);
      } else {
        updateKeyRow(tr, k);
      }
    }
    // Evict rows for keys the observer no longer reports (e.g. after a
    // reset_stats; or after the observer's idle-key eviction kicked in).
    Object.keys(rowByHint).forEach(function (hint) {
      if (!seen[hint]) {
        els.keyTbody.removeChild(rowByHint[hint]);
        delete rowByHint[hint];
      }
    });
  }

  function buildKeyRow(k) {
    var tr = document.createElement("tr");
    tr.dataset.keyHint = k.key_hint || "";
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

  // Point each slot's iframe at its provisioned Grafana panel. We embed
  // individual panels via /d-solo/<uid>/<slug>?panelId=N -- chromeless by
  // design (no breadcrumb, no sign-in button, no time picker), which is the
  // standard Grafana embed path. The slot's data-uid + data-panel-id tell
  // wireGrafanaSlots which panel to load. If grafana_url is missing we mark
  // the slot no-grafana and the CSS shows the fallback hint instead of a
  // broken iframe.
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
      var uid = slot.getAttribute("data-uid");
      var panelId = slot.getAttribute("data-panel-id");
      if (iframe && uid && panelId) {
        // The slug after the uid is decorative -- Grafana looks the dashboard
        // up by uid -- but the segment is required by the route.
        iframe.src = base.replace(/\/+$/, "") +
          "/d-solo/" + encodeURIComponent(uid) + "/_" +
          "?panelId=" + encodeURIComponent(panelId) +
          "&theme=dark&refresh=5s&from=now-15m&to=now&orgId=1";
      }
    }
  })();

  // ---------- test console (D3) -------------------------------------------
  //
  // Fetches /tests/list once on load, renders one card per case grouped by
  // category. Click "Run" on an automated card -> POST /tests/run/{id},
  // populate the card with the structured result (status, expected vs actual,
  // headers, body, details). Manual cards render an instructional Steps list
  // instead of a Run button. "Run all auto" iterates the automated cards
  // sequentially with the same UI updates per card.

  var testGrid = document.getElementById("test-grid");
  var btnRunAll = document.getElementById("btn-run-all");
  var testCatalog = [];            // list returned by /tests/list
  var cardByID = Object.create(null); // id -> DOM root for the card
  var runAllInFlight = false;

  var CATEGORY_TITLES = {
    happy:         "Happy paths",
    edge:          "Edge cases",
    observability: "Observability",
    audit:         "Audit-fix verifications",
    manual:        "Manual cards",
  };

  function loadCatalog() {
    fetch("/tests/list", { headers: { "Accept": "application/json" } })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (cases) {
        testCatalog = cases || [];
        renderCatalog();
      })
      .catch(function (err) {
        if (testGrid) testGrid.innerHTML = '<div class="empty">Failed to load test catalog: ' + escapeHTML(err.message) + '</div>';
      });
  }

  function renderCatalog() {
    if (!testGrid) return;
    testGrid.innerHTML = "";
    cardByID = Object.create(null);
    var order = ["happy", "edge", "observability", "audit", "manual"];
    order.forEach(function (cat) {
      var cases = testCatalog.filter(function (c) { return c.category === cat; });
      if (!cases.length) return;
      var head = document.createElement("h3");
      head.className = "test-cat";
      head.textContent = CATEGORY_TITLES[cat] || cat;
      testGrid.appendChild(head);
      var group = document.createElement("div");
      group.className = "test-cat-grid";
      cases.forEach(function (c) {
        var card = buildCard(c);
        cardByID[c.id] = card;
        group.appendChild(card);
      });
      testGrid.appendChild(group);
    });
  }

  function buildCard(c) {
    var card = document.createElement("div");
    card.className = "test-card";
    card.dataset.id = c.id;
    card.dataset.status = "idle";

    var header = document.createElement("div");
    header.className = "test-card-head";

    var title = document.createElement("strong");
    title.textContent = c.title;
    header.appendChild(title);

    var badge = document.createElement("span");
    badge.className = "test-badge test-badge-idle";
    badge.textContent = c.manual ? "manual" : "idle";
    header.appendChild(badge);
    card.appendChild(header);

    var desc = document.createElement("p");
    desc.className = "test-desc";
    desc.textContent = c.description;
    card.appendChild(desc);

    var actions = document.createElement("div");
    actions.className = "test-actions";

    if (c.manual) {
      // Manual cards render a Steps toggle if steps are provided.
      if (c.steps && c.steps.length) {
        var toggle = document.createElement("button");
        toggle.type = "button";
        toggle.className = "btn";
        toggle.textContent = "Show steps";
        var stepsList = document.createElement("ol");
        stepsList.className = "test-steps";
        stepsList.hidden = true;
        c.steps.forEach(function (s) {
          var li = document.createElement("li");
          li.textContent = s;
          stepsList.appendChild(li);
        });
        toggle.addEventListener("click", function () {
          stepsList.hidden = !stepsList.hidden;
          toggle.textContent = stepsList.hidden ? "Show steps" : "Hide steps";
        });
        actions.appendChild(toggle);
        card.appendChild(actions);
        card.appendChild(stepsList);
      } else {
        card.appendChild(actions);
      }
    } else {
      var runBtn = document.createElement("button");
      runBtn.type = "button";
      runBtn.className = "btn";
      runBtn.textContent = "Run";
      runBtn.addEventListener("click", function () { runTest(c.id); });
      actions.appendChild(runBtn);
      card.appendChild(actions);

      var resultBox = document.createElement("div");
      resultBox.className = "test-result";
      resultBox.hidden = true;
      card.appendChild(resultBox);
    }

    return card;
  }

  function runTest(id) {
    var card = cardByID[id];
    if (!card) return;
    if (card.dataset.status === "running") return;
    setCardStatus(card, "running");
    var resultBox = card.querySelector(".test-result");
    if (resultBox) {
      resultBox.hidden = true;
      resultBox.innerHTML = "";
    }
    var btn = card.querySelector(".test-actions .btn");
    if (btn) btn.disabled = true;

    return fetch("/tests/run/" + encodeURIComponent(id), { method: "POST" })
      .then(function (r) {
        return r.json().then(function (body) {
          if (!r.ok) throw new Error(body && body.error ? body.error : "HTTP " + r.status);
          return body;
        });
      })
      .then(function (res) {
        renderResult(card, res);
        setCardStatus(card, res.passed ? "pass" : "fail");
      })
      .catch(function (err) {
        renderResult(card, { passed: false, expected: "—", actual: "request error", details: err.message });
        setCardStatus(card, "fail");
      })
      .then(function () {
        if (btn) btn.disabled = false;
      });
  }

  function setCardStatus(card, status) {
    card.dataset.status = status;
    var badge = card.querySelector(".test-badge");
    if (!badge) return;
    badge.className = "test-badge test-badge-" + status;
    badge.textContent = status;
  }

  function renderResult(card, res) {
    var box = card.querySelector(".test-result");
    if (!box) return;
    box.hidden = false;
    box.innerHTML = "";

    var meta = document.createElement("div");
    meta.className = "test-result-meta";
    var ms = (res.duration_ms != null) ? (" · " + res.duration_ms + " ms") : "";
    var sc = (res.status_code != null && res.status_code !== 0) ? (" · HTTP " + res.status_code) : "";
    meta.textContent = (res.passed ? "PASS" : "FAIL") + ms + sc;
    box.appendChild(meta);

    if (res.expected) box.appendChild(kv("expected", res.expected));
    if (res.actual)   box.appendChild(kv("actual",   res.actual));
    if (res.details)  box.appendChild(kv("details",  res.details, true));

    if (res.headers && Object.keys(res.headers).length) {
      var hdrToggle = document.createElement("button");
      hdrToggle.type = "button";
      hdrToggle.className = "test-link";
      hdrToggle.textContent = "show headers (" + Object.keys(res.headers).length + ")";
      var hdrBlock = document.createElement("pre");
      hdrBlock.className = "test-pre";
      hdrBlock.hidden = true;
      hdrBlock.textContent = Object.keys(res.headers).sort().map(function (k) {
        return k + ": " + res.headers[k];
      }).join("\n");
      hdrToggle.addEventListener("click", function () {
        hdrBlock.hidden = !hdrBlock.hidden;
        hdrToggle.textContent = hdrBlock.hidden
          ? "show headers (" + Object.keys(res.headers).length + ")"
          : "hide headers";
      });
      box.appendChild(hdrToggle);
      box.appendChild(hdrBlock);
    }
    if (res.body) {
      var bodyToggle = document.createElement("button");
      bodyToggle.type = "button";
      bodyToggle.className = "test-link";
      bodyToggle.textContent = "show body";
      var bodyBlock = document.createElement("pre");
      bodyBlock.className = "test-pre";
      bodyBlock.hidden = true;
      bodyBlock.textContent = res.body;
      bodyToggle.addEventListener("click", function () {
        bodyBlock.hidden = !bodyBlock.hidden;
        bodyToggle.textContent = bodyBlock.hidden ? "show body" : "hide body";
      });
      box.appendChild(bodyToggle);
      box.appendChild(bodyBlock);
    }
  }

  function kv(label, value, multiline) {
    var wrap = document.createElement("div");
    wrap.className = "test-kv";
    var k = document.createElement("span");
    k.className = "test-kv-key";
    k.textContent = label;
    var v = document.createElement(multiline ? "pre" : "span");
    v.className = multiline ? "test-pre" : "test-kv-val";
    v.textContent = value;
    wrap.appendChild(k);
    wrap.appendChild(v);
    return wrap;
  }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  if (btnRunAll) {
    btnRunAll.addEventListener("click", function () {
      if (runAllInFlight) return;
      runAllInFlight = true;
      btnRunAll.disabled = true;
      var original = btnRunAll.textContent;
      var autoCases = testCatalog.filter(function (c) { return !c.manual; });
      var idx = 0;
      function next() {
        if (idx >= autoCases.length) {
          runAllInFlight = false;
          btnRunAll.disabled = false;
          btnRunAll.textContent = original;
          return;
        }
        var c = autoCases[idx++];
        btnRunAll.textContent = "Running " + idx + "/" + autoCases.length + "…";
        runTest(c.id).then(next, next);
      }
      next();
    });
  }

  loadCatalog();

  // ---------- scaling panel ------------------------------------------------
  //
  // Cards list every service in config/local-limits.yaml. CPU + memory usage
  // come from Prometheus via a same-origin /proxy/prom proxy (no CORS).
  // Allocated values come from the catalog the loadtest binary builds on
  // startup. Refreshed every 5 s.

  var scalingGrid = document.getElementById("scaling-grid");
  var scalingEnvBadge = document.getElementById("scaling-env-badge");
  var scalingCatalog = null;
  var scalingIsDockerDesktop = false;
  var SCALING_REFRESH_MS = 5000;

  function loadScalingCatalog() {
    if (!scalingGrid) return;
    fetch("/api/scaling-services")
      .then(function (r) { return r.ok ? r.json() : Promise.reject("HTTP " + r.status); })
      .then(function (data) {
        scalingCatalog = (data && data.services) || [];
        scalingIsDockerDesktop = !!(data && data.env && data.env.docker_desktop);
        renderScalingEnvBadge();
        if (scalingCatalog.length === 0) {
          scalingGrid.innerHTML = '<div class="empty">No services configured.</div>';
          return;
        }
        renderScalingCatalog();
        refreshScaling();
        setInterval(refreshScaling, SCALING_REFRESH_MS);
      })
      .catch(function (err) {
        scalingGrid.innerHTML = '<div class="empty">Scaling panel unavailable (' + escapeHTML(String(err)) + ')</div>';
      });
  }

  function renderScalingEnvBadge() {
    if (!scalingEnvBadge) return;
    if (!scalingIsDockerDesktop) { scalingEnvBadge.hidden = true; return; }
    scalingEnvBadge.hidden = false;
    scalingEnvBadge.textContent = "Docker Desktop";
    // Tooltip: on Docker Desktop, `docker stats` reports CPU% as % of all
    // host vCPUs in the Linux VM, so a container with --cpus 0.5 saturating
    // its cap can show e.g. 50% (= 0.5 cores). But unrelated VM activity can
    // also push the reading above 100% × alloc_cpu briefly; the bar caps
    // visually at 100% while the number stays truthful.
    scalingEnvBadge.title =
      "On Docker Desktop, `docker stats` reports CPU as a share of the Linux " +
      "VM's vCPUs, not the cap. Used cores may briefly exceed the allocated " +
      "value; the bar caps at 100% but the number stays truthful.";
  }

  function renderScalingCatalog() {
    scalingGrid.innerHTML = "";
    scalingCatalog.forEach(function (svc) {
      var card = document.createElement("div");
      card.className = "scaling-card";
      card.id = "scaling-card-" + svc.name;
      card.dataset.source = svc.source;
      // Numbers render as "used / allocated" (e.g. "0.40 / 0.50 cores") so a
      // Docker Desktop overshoot is visible even though the bar caps at 100%.
      card.innerHTML =
        '<div class="scaling-head">' +
          '<strong>' + escapeHTML(svc.name) + '</strong>' +
          '<span class="scaling-source">' + escapeHTML(svc.source) + '</span>' +
        '</div>' +
        '<div class="scaling-metric">' +
          '<div class="scaling-label">CPU <span class="scaling-num" data-field="cpu">— / ' + escapeHTML(formatCPU(svc.alloc_cpu)) + '</span></div>' +
          '<div class="scaling-bar"><div class="scaling-bar-fill" data-field="cpu-bar" style="width:0%"></div></div>' +
        '</div>' +
        '<div class="scaling-metric">' +
          '<div class="scaling-label">Memory <span class="scaling-num" data-field="mem">— / ' + svc.alloc_memory_mb + ' MB</span></div>' +
          '<div class="scaling-bar"><div class="scaling-bar-fill" data-field="mem-bar" style="width:0%"></div></div>' +
        '</div>';
      scalingGrid.appendChild(card);
    });
  }

  function refreshScaling() {
    if (!scalingCatalog) return;
    // Single round-trip: loadtest server-side queries Prometheus for host
    // binaries and shells `docker stats` for compose containers, returns one
    // combined payload. Per-service errors are reported in the row's `error`
    // field; we render "—" in that case.
    fetch("/api/scaling-stats")
      .then(function (r) { return r.ok ? r.json() : Promise.reject("HTTP " + r.status); })
      .then(function (data) {
        var stats = (data && data.stats) || [];
        var byName = {};
        stats.forEach(function (s) { byName[s.name] = s; });
        scalingCatalog.forEach(function (svc) {
          var s = byName[svc.name];
          if (!s || s.error) {
            updateScalingMetric(svc.name, "cpu", null, svc.alloc_cpu, formatCPUPair);
            updateScalingMetric(svc.name, "mem", null, svc.alloc_memory_mb, formatMemPair);
            return;
          }
          updateScalingMetric(svc.name, "cpu", s.cur_cpu_cores, svc.alloc_cpu, formatCPUPair);
          var mb = (s.cur_memory_bytes || 0) / 1024 / 1024;
          updateScalingMetric(svc.name, "mem", mb, svc.alloc_memory_mb, formatMemPair);
        });
      })
      .catch(function () { /* leave previous values in place on transient failure */ });
  }

  function updateScalingMetric(name, field, value, allocated, formatPairFn) {
    var card = document.getElementById("scaling-card-" + name);
    if (!card) return;
    var numEl = card.querySelector('[data-field="' + field + '"]');
    var barEl = card.querySelector('[data-field="' + field + '-bar"]');
    if (numEl) numEl.textContent = formatPairFn(value, allocated);
    if (!barEl) return;
    if (value == null || allocated <= 0) {
      barEl.style.width = "0%";
      barEl.classList.remove("hot", "warm");
      return;
    }
    // Bar caps visually at 100% even if value > allocated (Docker Desktop's
    // docker stats can briefly report this). Class triggers stay on the
    // clamped percentage so a saturated container glows red, not invisible.
    var pct = Math.min(100, (value / allocated) * 100);
    barEl.style.width = pct.toFixed(1) + "%";
    barEl.classList.toggle("hot", pct >= 100 || (value / allocated) > 1);
    if (pct < 100 && (value / allocated) <= 1) {
      barEl.classList.toggle("hot", pct > 80);
      barEl.classList.toggle("warm", pct > 50 && pct <= 80);
    } else {
      barEl.classList.remove("warm");
    }
  }

  // "X / Y cores" format: shows used and allocated together so an overshoot
  // (Docker Desktop quirk) is visible in the number even though the bar caps.
  function formatCPUPair(cur, alloc) {
    var allocStr = formatCPU(alloc);
    if (cur == null || isNaN(cur)) return "— / " + allocStr;
    return formatCPU(cur) + " / " + allocStr;
  }

  function formatMemPair(curMB, allocMB) {
    var allocStr = (allocMB != null) ? Math.round(allocMB) + " MB" : "—";
    if (curMB == null || isNaN(curMB)) return "— / " + allocStr;
    return Math.round(curMB) + " / " + allocStr;
  }

  function formatCPU(cores) {
    if (cores == null || isNaN(cores)) return "—";
    if (cores < 0.01) return "0 m";
    if (cores < 1) return Math.round(cores * 1000) + " m";
    return cores.toFixed(2) + " cores";
  }

  loadScalingCatalog();

  connect();
})();
