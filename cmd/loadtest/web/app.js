// ShortLink showcase page — vanilla JS, no build step.
//
// Piece 1 (this commit): wire up DOM placeholders so the page renders.
// Pieces 2-5 will fill in the WebSocket client, table rendering, log audit
// panel, filters, and command buttons. Each piece is its own commit.

(function () {
  "use strict";

  var cfg = window.SHORTLINK_CONFIG || {};

  // Surface the templated config in the dev console so it's easy to confirm
  // the Go server filled in the right OBSERVER / GRAFANA URLs.
  // eslint-disable-next-line no-console
  console.log("ShortLink showcase config:", cfg);

  // Until the WS client (piece 2) lands, just show the page is ready.
  var connText = document.getElementById("conn-text");
  if (connText) connText.textContent = "Ready (WS client lands in piece 2)";
})();
