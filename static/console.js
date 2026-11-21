/* Web console client.
   Ported from Pabnelia's wterm terminal (Apache-2.0, see js/wterm.LICENSE.txt).
   The wire protocol is unchanged: every frame is binary with a one-byte tag
   (0 = terminal input, 1 = JSON control). The backend runs `su - <user>` in an
   interactive Incus exec, so there is no SSH or password handling in here. */
(function () {
  "use strict";

  var FRAME_INPUT = 0;
  var FRAME_CONTROL = 1;

  var body = document.body;
  var host = document.getElementById("terminal");
  var statusEl = document.getElementById("term-status");
  var ledEl = document.getElementById("term-led");
  var sizeEl = document.getElementById("term-size");
  var flashTimer = null;
  var transport = null;
  var term = null;

  var instance = body.getAttribute("data-instance") || "instance";
  var wsPath = body.getAttribute("data-ws-path") || "/console/ws";
  var cols = parseInt(body.getAttribute("data-cols"), 10) || 80;
  var rows = parseInt(body.getAttribute("data-rows"), 10) || 24;
  var scheme = window.location.protocol === "https:" ? "wss://" : "ws://";
  var wsURL = scheme + window.location.host + wsPath +
    "?cols=" + encodeURIComponent(cols) + "&rows=" + encodeURIComponent(rows);

  function setStatus(text, level) {
    if (!statusEl) { return; }
    if (flashTimer) { window.clearTimeout(flashTimer); flashTimer = null; }
    statusEl.textContent = text;
    statusEl.className = "console-status" + (level ? " is-" + level : "");
    if (ledEl) { ledEl.className = "console-led" + (level ? " is-" + level : ""); }
  }

  function flashStatus(text, level) {
    if (!statusEl) { return; }
    var previous = statusEl.textContent;
    var previousLevel = statusEl.classList.contains("is-ok") ? "ok"
      : (statusEl.classList.contains("is-err") ? "err" : null);
    setStatus(text, level);
    if (flashTimer) { window.clearTimeout(flashTimer); }
    flashTimer = window.setTimeout(function () {
      flashTimer = null;
      setStatus(previous, previousLevel);
    }, 2600);
  }

  if (!host || !window.wterm || !window.wterm.WTerm || !window.wterm.WebSocketTransport) {
    setStatus("failed to load wterm", "err");
    return;
  }

  var encoder = new TextEncoder();

  function frame(tag, bytes) {
    // wterm occasionally emits an empty/undefined binary payload (for example
    // on a paste with no data). Never let that throw on bytes.length.
    var data;
    if (bytes instanceof Uint8Array) { data = bytes; }
    else if (typeof bytes === "string") { data = encoder.encode(bytes); }
    else { data = new Uint8Array(0); }
    var out = new Uint8Array(data.length + 1);
    out[0] = tag;
    out.set(data, 1);
    return out;
  }

  // Track DECSET/DECRST 2004 so pastes use bracketed mode when the foreground
  // application (nano, vim, ...) asked for it.
  var bracketedPaste = false;
  var sniffTail = "";
  var sniffDecoder = new TextDecoder();
  function sniffBracketed(chunk) {
    var text = typeof chunk === "string" ? chunk : null;
    if (text === null) {
      try { text = sniffDecoder.decode(chunk); } catch (e) { return; }
    }
    var buf = sniffTail + text;
    sniffTail = buf.slice(-16);
    var re = /\x1b\[\?([0-9;]*)[hl]/g;
    var match;
    while ((match = re.exec(buf)) !== null) {
      var params = match[1].split(";");
      for (var i = 0; i < params.length; i++) {
        if (params[i] === "2004") { bracketedPaste = match[0].slice(-1) === "h"; }
      }
    }
  }

  function layout() {
    var top = host.getBoundingClientRect().top;
    // The footer (and its SSH notice) sits outside .console-body. Measuring
    // it keeps the terminal above the footer instead of spilling over it.
    var foot = document.querySelector(".console-foot");
    var footHeight = foot ? foot.getBoundingClientRect().height : 0;
    var available = window.innerHeight - top - footHeight;
    host.style.height = Math.max(160, available) + "px";
  }

  // A path inserted from the file panel must never be able to carry terminal
  // control sequences or a newline into the shell. Strip every C0 control
  // character and cap the length before sending it as typed input.
  function sanitizeInsertText(text) {
    return String(text).replace(/[\x00-\x1f\x7f]/g, "").slice(0, 4096);
  }

  transport = new window.wterm.WebSocketTransport({
    url: wsURL,
    reconnect: false,
    onData: function (data) { sniffBracketed(data); term.write(data); },
    onOpen: function () {
      setStatus("connected", "ok");
      // The file panel waits for a working shell before it connects.
      window.dispatchEvent(new CustomEvent("console:connected"));
    },
    onClose: function () { setStatus("disconnected — check session or instance state", "err"); },
    onError: function () { setStatus("connection error", "err"); }
  });

  layout();

  term = new window.wterm.WTerm(host, {
    wasmUrl: "/static/wasm/wterm.wasm",
    autoResize: true,
    onData: function (data) { transport.send(frame(FRAME_INPUT, encoder.encode(data))); },
    onBinary: function (bytes) { transport.send(frame(FRAME_INPUT, bytes)); },
    onResize: function (c, r) {
      if (sizeEl) { sizeEl.textContent = c + "x" + r; }
      transport.send(frame(FRAME_CONTROL, encoder.encode(JSON.stringify({ t: "r", c: c, r: r }))));
    },
    onTitle: function (title) {
      if (title) { document.title = title + " — " + instance; }
    }
  });

  term.init().then(function () {
    layout();
    term.fit();
    term.focus();
    transport.connect();
  }).catch(function (err) {
    var detail = (err && (err.message || err.toString())) || "unknown error";
    setStatus("terminal failed to start: " + detail, "err");
    if (window.console && console.error) { console.error("wterm init failed:", err); }
  });

  document.getElementById("term-reconnect").addEventListener("click", function () {
    window.location.reload();
  });
  document.getElementById("term-clear").addEventListener("click", function () {
    transport.send(frame(FRAME_INPUT, encoder.encode("\u000c")));
    term.focus();
  });
  document.getElementById("term-close").addEventListener("click", function () {
    window.close();
  });

  function pasteText(text) {
    if (!text || !transport) { return; }
    // Drop ESC so clipboard content cannot inject terminal control sequences.
    var clean = String(text).replace(/\x1b/g, "");
    if (!clean) { return; }
    if (bracketedPaste) { clean = "\x1B[200~" + clean + "\x1B[201~"; }
    try { transport.send(frame(FRAME_INPUT, encoder.encode(clean))); } catch (e) { /* already disconnected */ }
    if (term) { term.focus(); }
  }

  function pasteFromClipboard() {
    if (!navigator.clipboard || !navigator.clipboard.readText) {
      flashStatus("paste needs https — click the terminal, then Ctrl+V", "err");
      if (term) { term.focus(); }
      return;
    }
    navigator.clipboard.readText().then(function (text) {
      if (text) { pasteText(text); } else if (term) { term.focus(); }
    }).catch(function () {
      flashStatus("clipboard blocked — click the terminal, then Ctrl+V", "err");
      if (term) { term.focus(); }
    });
  }

  document.getElementById("term-paste").addEventListener("click", pasteFromClipboard);

  // Capture before wterm's textarea sees Ctrl+Shift+V (otherwise it becomes ^V).
  host.addEventListener("keydown", function (e) {
    var mod = e.ctrlKey || e.metaKey;
    var shiftV = mod && e.shiftKey && !e.altKey && (e.key === "V" || e.key === "v");
    var shiftIns = !mod && !e.altKey && e.shiftKey && e.key === "Insert";
    if (shiftV || shiftIns) {
      e.preventDefault();
      e.stopPropagation();
      pasteFromClipboard();
    }
  }, true);

  // Right-click copies a selection, pastes otherwise, like a native terminal.
  host.addEventListener("contextmenu", function (e) {
    e.preventDefault();
    var selection = "";
    try { selection = (term && term.getSelectionText && term.getSelectionText()) || ""; } catch (err) { selection = ""; }
    if (selection) {
      try {
        if (document.execCommand("copy")) { flashStatus("copied " + selection.length + " chars", "ok"); }
        else { flashStatus("copy failed — use Ctrl+Shift+C", "err"); }
      } catch (err) {
        flashStatus("copy failed — use Ctrl+Shift+C", "err");
      }
      return;
    }
    pasteFromClipboard();
  });

  window.addEventListener("resize", function () {
    layout();
    if (term) { try { term.fit(); } catch (e) { /* noop */ } }
  });

  // The collapsible file panel changes the terminal's width without a window
  // resize, so refit the PTY explicitly and hand focus back to the shell.
  window.addEventListener("console:relayout", function () {
    layout();
    if (term) {
      try { term.fit(); } catch (e) { /* noop */ }
      try { term.focus(); } catch (e) { /* noop */ }
    }
  });

  // "Copy path to terminal" from the file panel types into the same shell.
  window.addEventListener("sftp:insert", function (event) {
    var text = sanitizeInsertText(event.detail && event.detail.text);
    if (!text || !transport) { return; }
    // Bracketed paste keeps a path containing shell metacharacters from being
    // interpreted as keystrokes by the line editor.
    var payload = bracketedPaste ? "\x1B[200~" + text + "\x1B[201~" : text;
    try { transport.send(frame(FRAME_INPUT, encoder.encode(payload))); } catch (e) { /* disconnected */ }
    if (term) { try { term.focus(); } catch (e) { /* noop */ } }
  });

  window.addEventListener("beforeunload", function () {
    try { transport.close(); } catch (e) { /* noop */ }
    try { term.destroy(); } catch (e) { /* noop */ }
  });
})();
