/* Thin SFTP file panel for the web console.
   Self-contained: the standalone console page loads no htmx or app.js.
   All data comes from data-* attributes and the same-origin SFTP websocket. */
(function () {
  "use strict";

  var panel = document.getElementById("sftp-panel");
  if (!panel) { return; }
  var wsPath = document.body.getAttribute("data-sftp-ws-path");

  var listEl = document.getElementById("sftp-list");
  var pathEl = document.getElementById("sftp-path");
  var stateEl = document.getElementById("sftp-state");
  var statusEl = document.getElementById("sftp-status");
  var fileInput = document.getElementById("sftp-file");
  var toggleBtn = document.getElementById("sftp-toggle");

  var CHUNK = 256 * 1024;
  var ws = null;
  var cwd = "";
  var root = "";
  var entries = [];
  var selected = new Set();
  var clip = { paths: [], cut: false };
  var seq = 1;
  // Transfer ids live in a separate range from control request ids so an
  // upload chunk ack can never be mistaken for a control response.
  var xfer = 1000000;
  var pending = {};
  var writeWaits = {};
  // Download transfers are streamed by the server: id -> preallocated buffer.
  var downloads = {};

  function setState(text, kind) {
    stateEl.textContent = text;
    stateEl.className = "sftp-state" + (kind ? " is-" + kind : "");
  }
  function setStatus(text, kind) {
    statusEl.textContent = text || "";
    statusEl.className = "sftp-status" + (kind ? " is-" + kind : "");
  }
  function fmtSize(n) {
    if (n < 1024) { return n + " B"; }
    if (n < 1048576) { return (n / 1024).toFixed(1) + " K"; }
    if (n < 1073741824) { return (n / 1048576).toFixed(1) + " M"; }
    return (n / 1073741824).toFixed(1) + " G";
  }
  function joinPath(dir, name) {
    if (dir === "/") { return "/" + name; }
    return dir + "/" + name;
  }
  function displayPath(p) {
    if (!p) { return "~"; }
    if (p === root) { return "~"; }
    if (root && p.indexOf(root + "/") === 0) { return "~/" + p.slice(root.length + 1); }
    return p;
  }
  function cleanName(name) {
    return String(name).replace(/[\x00-\x1f\x7f]/g, "").trim().slice(0, 255);
  }
  function shellQuote(p) {
    var safe = String(p).replace(/[\x00-\x1f\x7f]/g, "");
    return "'" + safe.replace(/'/g, "'\\''") + "'";
  }
  function safeFileName(name) {
    var base = String(name).split("/").pop().replace(/[\x00-\x1f\x7f]/g, "").trim();
    return base || "download";
  }
  function withTimeout(promise, ms) {
    return new Promise(function (resolve, reject) {
      var done = false;
      var t = setTimeout(function () {
        if (!done) { done = true; reject(new Error("transfer timed out")); }
      }, ms);
      promise.then(function (v) {
        if (!done) { done = true; clearTimeout(t); resolve(v); }
      }, function (e) {
        if (!done) { done = true; clearTimeout(t); reject(e); }
      });
    });
  }

  function request(obj, timeoutMs) {
    return new Promise(function (resolve, reject) {
      if (!ws || ws.readyState !== 1) { reject(new Error("file panel is not connected")); return; }
      if (!obj.id) { obj.id = seq++; }
      var t = setTimeout(function () {
        if (pending[obj.id]) { delete pending[obj.id]; reject(new Error("request timed out")); }
      }, timeoutMs || 30000);
      pending[obj.id] = function (msg) { clearTimeout(t); resolve(msg); };
      try { ws.send(JSON.stringify(obj)); }
      catch (e) { clearTimeout(t); delete pending[obj.id]; reject(e); }
    });
  }

  function connect() {
    if (ws && ws.readyState <= 1) { return; }
    if (!wsPath) { setState("file panel unavailable", "err"); return; }
    setState("connecting…");
    var scheme = window.location.protocol === "https:" ? "wss://" : "ws://";
    ws = new WebSocket(scheme + window.location.host + wsPath);
    ws.binaryType = "arraybuffer";

    ws.onopen = function () { setState("connected", "ok"); };

    ws.onmessage = function (event) {
      if (typeof event.data === "string") {
        var msg;
        try { msg = JSON.parse(event.data); } catch (e) { return; }
        if (msg.op === "download_done" && downloads[msg.id]) {
          var done = downloads[msg.id];
          delete downloads[msg.id];
          if (done.end !== done.size) {
            done.reject(new Error("download incomplete: received " + done.end + " of " + done.size + " bytes"));
            return;
          }
          saveBlob(new Blob([done.buf]), done.name);
          done.resolve(done.name);
          return;
        }
        if (msg.op === "download_error" && downloads[msg.id]) {
          var failed = downloads[msg.id];
          delete downloads[msg.id];
          failed.reject(new Error(msg.error || "download failed"));
          return;
        }
        if (msg.id && pending[msg.id]) {
          var cb = pending[msg.id];
          delete pending[msg.id];
          cb(msg);
          return;
        }
        if (msg.op === "write" && writeWaits[msg.id]) {
          var w = writeWaits[msg.id];
          delete writeWaits[msg.id];
          if (msg.ok) { w.resolve(); } else { w.reject(new Error(msg.error || "write failed")); }
          return;
        }
        if (msg.op === "error") { setStatus(msg.error || "file panel error", true); return; }
        if (msg.op === "ready") {
          root = msg.cwd || "";
          setState("connected", "ok");
          load("~");
          return;
        }
        return;
      }

      // Binary frame: [8-byte transfer id][8-byte offset][payload].
      // Place the payload at its absolute offset in the preallocated file
      // buffer; order of arrival does not matter, so transfers can interleave.
      var buf = new Uint8Array(event.data);
      if (buf.byteLength < 16) { return; }
      var view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
      var id = Number(view.getBigUint64(0, false));
      var offset = Number(view.getBigUint64(8, false));
      var payload = buf.slice(16);
      var transfer = downloads[id];
      if (transfer && payload.length && offset + payload.length <= transfer.buf.length) {
        transfer.buf.set(payload, offset);
        transfer.end = Math.max(transfer.end, offset + payload.length);
        setStatus("Downloading " + transfer.name + " — " + Math.round(transfer.end * 100 / (transfer.size || 1)) + "%");
      }
    };

    ws.onclose = function () {
      setState("disconnected", "err");
      ws = null;
      Object.keys(downloads).forEach(function (id) {
        downloads[id].reject(new Error("file panel disconnected"));
      });
      pending = {}; writeWaits = {}; downloads = {};
    };
    ws.onerror = function () { setState("connection error", "err"); };
  }

  function updateActions() {
    var n = selected.size;
    ["sftp-copy", "sftp-delete", "sftp-download", "sftp-copypath", "sftp-terminal"].forEach(function (id) {
      var el = document.getElementById(id);
      if (el) { el.disabled = n === 0; }
    });
    var paste = document.getElementById("sftp-paste");
    if (paste) { paste.disabled = clip.paths.length === 0; }
  }

  function render() {
    listEl.replaceChildren();
    if (!entries.length) {
      var empty = document.createElement("div");
      empty.className = "sftp-empty";
      empty.textContent = "This folder is empty.";
      listEl.appendChild(empty);
      updateActions();
      return;
    }
    entries.forEach(function (e) {
      var row = document.createElement("div");
      row.className = "sftp-row" + (selected.has(e.path) ? " selected" : "");

      var cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = selected.has(e.path);
      cb.setAttribute("aria-label", "Select " + e.name);
      cb.addEventListener("change", function () {
        if (cb.checked) { selected.add(e.path); } else { selected.delete(e.path); }
        row.classList.toggle("selected", cb.checked);
        updateActions();
      });

      var name = document.createElement("span");
      name.className = "sftp-name" + (e.dir ? " dir" : "");
      name.textContent = (e.dir ? "▸ " : "") + e.name;
      name.title = e.path;
      if (e.dir) {
        name.addEventListener("click", function () { navigate(e.path); });
      }

      var meta = document.createElement("span");
      meta.className = "sftp-meta";
      meta.textContent = e.dir ? "" : fmtSize(e.size);

      row.appendChild(cb);
      row.appendChild(name);
      row.appendChild(meta);
      listEl.appendChild(row);
    });
    updateActions();
  }

  function load(p) {
    return request({ op: "list", path: p || "~" }).then(function (msg) {
      cwd = msg.cwd || cwd;
      entries = msg.entries || [];
      selected.clear();
      pathEl.textContent = displayPath(cwd);
      pathEl.title = cwd;
      render();
      setStatus("");
    }).catch(function (err) { setStatus(err.message, true); });
  }

  function navigate(p) { if (p) { return load(p); } }

  // The server streams the whole file after download_begin, so there is one
  // request per file rather than one round trip per chunk. Chunks are placed
  // by absolute offset, which makes interleaved/parallel downloads safe.
  function downloadOne(entry) {
    var id = xfer++;
    return request({ op: "download_begin", id: id, path: entry.path }).then(function (msg) {
      if (!msg || msg.ok !== true || typeof msg.size !== "number") {
        throw new Error("invalid download response");
      }
      return new Promise(function (resolve, reject) {
        var size = msg.size;
        var timer = setTimeout(function () {
          if (downloads[id]) {
            delete downloads[id];
            reject(new Error("download timed out"));
          }
        }, 120000);
        downloads[id] = {
          buf: new Uint8Array(size),
          size: size,
          end: 0,
          name: safeFileName(entry.name),
          resolve: function (name) { clearTimeout(timer); resolve(name); },
          reject: function (err) { clearTimeout(timer); reject(err); }
        };
        setStatus("Downloading " + entry.name + "…");
      });
    });
  }

  async function uploadOne(file) {
    var id = xfer++;
    await request({ op: "upload_begin", id: id, path: joinPath(cwd, file.name), size: file.size });
    var offset = 0;
    while (offset < file.size) {
      var slice = new Uint8Array(await file.slice(offset, offset + CHUNK).arrayBuffer());
      var frame = new Uint8Array(16 + slice.length);
      var view = new DataView(frame.buffer);
      view.setBigUint64(0, BigInt(id));
      view.setBigUint64(8, BigInt(offset));
      frame.set(slice, 16);
      var ack = new Promise(function (resolve, reject) { writeWaits[id] = { resolve: resolve, reject: reject }; });
      ws.send(frame);
      await withTimeout(ack, 60000);
      offset += slice.length;
      setStatus("Uploading " + file.name + " — " + Math.round(offset * 100 / (file.size || 1)) + "%");
    }
    await request({ op: "upload_end", id: id });
    return file.name;
  }

  // A small pool gives parallel, multi-threaded transfers without flooding
  // the socket with an unbounded number of files at once.
  async function pool(items, limit, worker) {
    var index = 0;
    async function run() {
      while (index < items.length) {
        var item = items[index++];
        await worker(item);
      }
    }
    var runners = [];
    for (var i = 0; i < Math.min(limit, items.length); i++) { runners.push(run()); }
    await Promise.all(runners);
  }

  function saveBlob(blob, name) {
    var url = URL.createObjectURL(blob);
    var a = document.createElement("a");
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    setTimeout(function () { URL.revokeObjectURL(url); }, 2000);
  }

  function selectedEntries() {
    return entries.filter(function (e) { return selected.has(e.path); });
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () { setStatus("Copied path", false); }, function () { fallbackCopy(text); });
    } else { fallbackCopy(text); }
  }
  function fallbackCopy(text) {
    var area = document.createElement("textarea");
    area.value = text;
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    try { document.execCommand("copy"); setStatus("Copied path"); }
    catch (e) { setStatus("Clipboard blocked", true); }
    document.body.removeChild(area);
  }

  // --- button wiring -------------------------------------------------------

  document.getElementById("sftp-toggle").addEventListener("click", function () {
    panel.classList.toggle("collapsed");
    var collapsed = panel.classList.contains("collapsed");
    toggleBtn.textContent = collapsed ? "›" : "‹";
    toggleBtn.title = collapsed ? "Expand file panel" : "Collapse file panel";
    toggleBtn.setAttribute("aria-label", toggleBtn.title);
    window.dispatchEvent(new CustomEvent("console:relayout"));
  });

  document.getElementById("sftp-up").addEventListener("click", function () {
    if (!cwd || cwd === root) { return; }
    var parent = cwd.slice(0, cwd.lastIndexOf("/"));
    load(parent || root);
  });
  document.getElementById("sftp-refresh").addEventListener("click", function () { load(cwd); });

  document.getElementById("sftp-new").addEventListener("click", function () {
    if (!ws || ws.readyState !== 1) { return; }
    var name = cleanName(window.prompt("Folder name"));
    if (!name || name === "." || name === ".." || name.indexOf("/") >= 0) {
      setStatus("Invalid folder name", true);
      return;
    }
    request({ op: "mkdir", path: cwd, dest: name }).then(function () {
      setStatus("Folder created");
      return load(cwd);
    }).catch(function (err) { setStatus(err.message, true); });
  });

  fileInput.addEventListener("change", function () {
    var files = Array.prototype.slice.call(fileInput.files || []);
    fileInput.value = "";
    if (!files.length) { return; }
    setStatus("Uploading " + files.length + " file(s)…");
    pool(files, 3, uploadOne).then(function (names) {
      setStatus("Uploaded " + names.length + " file(s)", false);
      return load(cwd);
    }).catch(function (err) { setStatus(err.message, true); load(cwd); });
  });
  document.getElementById("sftp-upload").addEventListener("click", function () { fileInput.click(); });

  document.getElementById("sftp-copy").addEventListener("click", function () {
    clip = { paths: selectedEntries().map(function (e) { return e.path; }), cut: false };
    updateActions();
    setStatus(clip.paths.length + " item(s) copied");
  });
  document.getElementById("sftp-paste").addEventListener("click", function () {
    if (!clip.paths.length) { return; }
    request({ op: "copy", paths: clip.paths, dest: cwd }).then(function () {
      setStatus("Pasted " + clip.paths.length + " item(s)");
      return load(cwd);
    }).catch(function (err) { setStatus(err.message, true); });
  });
  document.getElementById("sftp-delete").addEventListener("click", function () {
    var items = selectedEntries();
    if (!items.length) { return; }
    if (!window.confirm("Delete " + items.length + " selected item(s)? This cannot be undone.")) { return; }
    request({ op: "remove", paths: items.map(function (e) { return e.path; }) }).then(function () {
      setStatus("Deleted " + items.length + " item(s)");
      return load(cwd);
    }).catch(function (err) { setStatus(err.message, true); });
  });
  document.getElementById("sftp-download").addEventListener("click", function () {
    var items = selectedEntries().filter(function (e) { return !e.dir; });
    if (!items.length) { setStatus("Select one or more files to download", true); return; }
    setStatus("Preparing " + items.length + " download(s)…");
    pool(items, 3, downloadOne).then(function (names) {
      setStatus("Downloaded " + names.length + " file(s)");
    }).catch(function (err) { setStatus(err.message, true); });
  });
  document.getElementById("sftp-copypath").addEventListener("click", function () {
    var items = selectedEntries();
    copyText(items.length ? items[0].path : cwd);
  });
  document.getElementById("sftp-terminal").addEventListener("click", function () {
    var items = selectedEntries();
    if (!items.length) { return; }
    window.dispatchEvent(new CustomEvent("sftp:insert", { detail: { text: shellQuote(items[0].path) } }));
    setStatus("Path inserted into the terminal");
  });

  // The panel only opens once the terminal shell is actually connected.
  window.addEventListener("console:connected", function () {
    panel.classList.remove("collapsed");
    toggleBtn.textContent = "‹";
    window.dispatchEvent(new CustomEvent("console:relayout"));
    connect();
  });
})();
