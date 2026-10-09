/* Peaceful Cloud Console — minimal progressive enhancement.
   Everything important works without JavaScript; this only adds conveniences. */
(function () {
  "use strict";

  // Inline event handlers are blocked by the page's Content-Security-Policy,
  // so the two things they did live here.
  document.addEventListener("click", function (event) {
    var field = event.target.closest("input[data-select-on-click]");
    if (field) {
      field.select();
    }
  });

  document.addEventListener("htmx:afterRequest", function (event) {
    var form = event.target;
    if (form && form.matches && form.matches("form[data-reset-after-request]")) {
      form.reset();
    }
  });

  // Sidebar toggle on narrow screens.
  document.addEventListener("click", function (event) {
    var toggle = event.target.closest("#sidebar-toggle");
    if (toggle) {
      var sidebar = document.getElementById("sidebar");
      if (sidebar) {
        // Narrow overlay uses .open; wide layout collapses via body class
        // (desktop .sidebar.open has no rule, which is why the button
        // previously appeared to do nothing).
        if (window.matchMedia("(max-width: 860px)").matches) {
          sidebar.classList.toggle("open");
          toggle.setAttribute("aria-expanded", sidebar.classList.contains("open") ? "true" : "false");
        } else {
          document.body.classList.toggle("sidebar-collapsed");
          toggle.setAttribute("aria-expanded", document.body.classList.contains("sidebar-collapsed") ? "false" : "true");
        }
      }
      return;
    }

    // Clicking outside the overlay dismisses it on narrow screens.
    if (window.matchMedia("(max-width: 860px)").matches) {
      var openSidebar = document.getElementById("sidebar");
      if (openSidebar && openSidebar.classList.contains("open") &&
          !event.target.closest("#sidebar") && !event.target.closest("#sidebar-toggle")) {
        openSidebar.classList.remove("open");
      }
    }

    // Copy buttons: <button data-copy="text"> or data-copy-target="#selector".
    var copyBtn = event.target.closest("[data-copy], [data-copy-target]");
    if (copyBtn) {
      var text = copyBtn.getAttribute("data-copy");
      if (!text) {
        var target = document.querySelector(copyBtn.getAttribute("data-copy-target"));
        text = target ? (target.value !== undefined ? target.value : target.textContent) : "";
      }
      if (!text) {
        return;
      }
      var done = function () {
        var original = copyBtn.textContent;
        copyBtn.textContent = "Copied";
        setTimeout(function () { copyBtn.textContent = original; }, 1400);
      };
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(done, function () {});
      } else {
        var area = document.createElement("textarea");
        area.value = text;
        area.style.position = "fixed";
        area.style.opacity = "0";
        document.body.appendChild(area);
        area.select();
        try { document.execCommand("copy"); done(); } catch (e) { /* ignore */ }
        document.body.removeChild(area);
      }
      return;
    }

    // Close open dropdown menus when clicking elsewhere.
    if (!event.target.closest("details.menu")) {
      document.querySelectorAll("details.menu[open]").forEach(function (menu) {
        menu.removeAttribute("open");
      });
    }
  });

  // Destructive actions ask for confirmation. Forms submitted via HTMX carry
  // hx-confirm so HTMX prompts instead — confirming twice would be worse.
  // (Without HTMX loaded, hx-* attributes are inert and this still applies.)
  document.addEventListener("submit", function (event) {
    var form = event.target.closest("form[data-confirm]");
    if (!form) {
      return;
    }
    if (window.htmx && (form.hasAttribute("hx-post") || form.hasAttribute("hx-get"))) {
      return;
    }
    if (!window.confirm(form.getAttribute("data-confirm"))) {
      event.preventDefault();
    }
  });

  // Instance detail tabs are HTMX-swapped into #tab-panel only, so the
  // server-rendered `active` class would never move. Keep it in sync here.
  // Full page loads already render the right tab as active.
  document.addEventListener("click", function (event) {
    var tab = event.target.closest(".tabs .tab");
    if (!tab) {
      return;
    }
    var container = tab.closest(".tabs");
    if (container) {
      container.querySelectorAll(".tab.active").forEach(function (el) {
        el.classList.remove("active");
      });
      tab.classList.add("active");
    }
  });

  // Reveal a form while a long request is in flight.
  document.addEventListener("htmx:beforeRequest", function (event) {
    var el = event.detail.elt;
    if (el && el.tagName === "FORM") {
      el.setAttribute("aria-busy", "true");
    }
  });
  document.addEventListener("htmx:afterRequest", function (event) {
    var el = event.detail.elt;
    if (el && el.tagName === "FORM") {
      el.removeAttribute("aria-busy");
    }
  });
  // Same drain-to-reload for the HTMX polling fallback (no EventSource).
  document.addEventListener("htmx:afterSwap", function (event) {
    var target = event.detail && event.detail.target;
    if (target && target.id === "job-tracker") {
      noteTrackerState(!!target.querySelector(".job"));
    }
  });

  // Instance lifecycle actions (stop/start/reboot) swap in place instead of
  // reloading the page. The POST response already carries fresh buttons and
  // status via out-of-band swaps; the tab panel below still shows the old
  // state, and slow fields (address, uptime) converge a little later, so
  // refresh both here without a full reload. Deletes navigate via
  // HX-Redirect and need no handling.
  var headerPollTimers = [];
  function refreshInstanceTab() {
    var panel = document.getElementById("tab-panel");
    if (!panel || !window.htmx) {
      return;
    }
    var active = document.querySelector(".tabs .tab.active");
    var url = active && active.getAttribute("hx-get");
    if (!url) {
      return;
    }
    window.htmx.ajax("GET", url, { target: "#tab-panel", swap: "innerHTML" });
  }
  function pollInstanceHeader(name) {
    if (!name || !window.htmx) {
      return;
    }
    headerPollTimers.forEach(function (timer) { clearTimeout(timer); });
    headerPollTimers = [];
    // The header endpoint renders the same out-of-band fragments, so each
    // poll swaps the actions and status in place (swap none: only the OOB
    // parts apply).
    [2000, 6000, 12000, 25000].forEach(function (delay) {
      headerPollTimers.push(setTimeout(function () {
        if (!document.getElementById("instance-actions")) {
          return;
        }
        window.htmx.ajax("GET", "/instances/" + encodeURIComponent(name) + "/header", { swap: "none" });
      }, delay));
    });
  }
  function instanceNameFromRequest(elt) {
    if (!elt || !elt.getAttribute) {
      return null;
    }
    var url = elt.getAttribute("hx-post") || elt.getAttribute("action") || "";
    var match = url.match(/\/instances\/([^\/]+)\/state/);
    return match ? decodeURIComponent(match[1]) : null;
  }
  document.addEventListener("htmx:afterRequest", function (event) {
    var detail = event.detail || {};
    if (!detail.successful) {
      return;
    }
    var name = instanceNameFromRequest(detail.elt);
    if (!name) {
      return;
    }
    refreshInstanceTab();
    pollInstanceHeader(name);
  });
  // The state response also triggers this event via HX-Trigger; refresh from
  // the current path in case a future form stops matching the pattern above.
  document.addEventListener("instance-updated", function () {
    refreshInstanceTab();
    var match = window.location.pathname.match(/^\/instances\/([^\/]+)/);
    if (match && match[1]) {
      pollInstanceHeader(decodeURIComponent(match[1]));
    }
  });

  // AJAX flash messages (out-of-band #flash-wrap / #settings-flash swaps)
  // clear themselves; full page loads keep theirs until the next navigation.
  document.addEventListener("htmx:oobAfterSwap", function (event) {
    var target = event.detail && event.detail.target;
    if (!target) {
      return;
    }
    // After a wrong 2FA code the form swaps in place with a fresh challenge;
    // put the cursor back in the code field.
    if (target.id === "totp-form") {
      var input = target.querySelector('input[name="code"]');
      if (input) {
        input.focus();
        input.select();
      }
      return;
    }
    if (target.id !== "flash-wrap" && target.id !== "settings-flash" && target.id !== "login-flash") {
      return;
    }
    if (target.textContent.trim() === "") {
      return;
    }
    setTimeout(function () {
      if (document.body.contains(target)) {
        target.innerHTML = "";
      }
    }, 6000);
  });

  // The job tracker polls itself; stop polling once every job has finished by
  // letting the server render an idle tracker with a slow interval.
  //
  // Live updates via Server-Sent Events (inspect in DevTools under
  // Network > EventStream on GET /jobs/stream). The server keeps one
  // long-lived request open and pushes the same job_list partial only when
  // jobs change, instead of HTMX polling GET /jobs/active every 2s/15s.
  // No EventSource (or a 401/stream error) falls back to that polling.
  //
  // The tracker only shows active jobs, so a finished job vanishes from it
  // while the instance list on the page stays stale. When the tracker drains
  // from having jobs to having none, reload once so the new instance (or the
  // fresh state after a rebuild/delete elsewhere) appears without a manual
  // refresh.
  var initialTracker = document.getElementById("job-tracker");
  var sawActiveJobs = !!(initialTracker && initialTracker.querySelector(".job"));
  var jobReloadScheduled = false;
  function noteTrackerState(hasJobs) {
    if (hasJobs) {
      sawActiveJobs = true;
      return;
    }
    if (sawActiveJobs && !jobReloadScheduled) {
      jobReloadScheduled = true;
      setTimeout(function () { window.location.reload(); }, 1500);
    }
  }
  function swapJobTracker(html) {
    var current = document.getElementById("job-tracker");
    if (!current) {
      return;
    }
    var tpl = document.createElement("template");
    tpl.innerHTML = html.trim();
    var next = tpl.content.querySelector("#job-tracker");
    if (!next) {
      return;
    }
    // The stream replaces polling: strip the HTMX poll attributes from the
    // incoming node so replacing it also kills the old polling timer.
    next.removeAttribute("hx-get");
    next.removeAttribute("hx-trigger");
    next.removeAttribute("hx-swap");
    next.removeAttribute("hx-target");
    noteTrackerState(!!next.querySelector(".job"));
    current.replaceWith(next);
    if (window.htmx && typeof window.htmx.process === "function") {
      window.htmx.process(next);
    }
  }

  function connectJobStream() {
    var tracker = document.getElementById("job-tracker");
    if (!tracker || typeof window.EventSource === "undefined") {
      return;
    }
    var url = tracker.getAttribute("data-jobs-stream") || "/jobs/stream";
    var source;
    try {
      source = new window.EventSource(url);
    } catch (e) {
      return;
    }
    source.addEventListener("jobs", function (event) {
      if (event && typeof event.data === "string") {
        swapJobTracker(event.data);
      }
    });
    // On auth expiry or network failure the server answers 401/closes the
    // stream; stop retrying and leave the HTMX polling fallback in place.
    source.addEventListener("error", function () {
      if (source.readyState === window.EventSource.CLOSED) {
        source.close();
      }
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", connectJobStream);
  } else {
    connectJobStream();
  }

  // Reveal the custom resource fields when the "Custom" plan is selected.
  function syncCustomPlan() {
    var custom = document.querySelector('input[name="size"]:checked');
    if (!custom) {
      return;
    }

    var disclosure = document.querySelector("details.disclosure");
    if (!disclosure) {
      return;
    }

    if (custom.value === "custom") {
      disclosure.setAttribute("open", "");
    } else {
      disclosure.removeAttribute("open");
    }
  }

  document.addEventListener("change", function (event) {
    if (event.target.closest('input[name="size"]')) {
      syncCustomPlan();
    }
    if (event.target.closest('input[name="platform"]')) {
      syncInstanceType();
    }
  });
  syncCustomPlan();
  syncInstanceType();

  // Application images are container-only; disable the VM option while the
  // apps platform is selected. The server validates this regardless.
  function syncInstanceType() {
    var platform = document.querySelector('input[name="platform"]:checked');
    var vm = document.querySelector('input[name="kind"][value="virtual-machine"]');
    var container = document.querySelector('input[name="kind"][value="container"]');
    var hint = document.getElementById("kind-hint");
    if (!vm || !container) {
      return;
    }
    if (platform && platform.value === "apps") {
      vm.disabled = true;
      container.checked = true;
      if (hint) {
        hint.textContent = "The selected application image runs as a container only.";
      }
      return;
    }
    vm.disabled = false;
    if (hint) {
      hint.textContent = "Application images run as containers only; operating systems run as either type.";
    }
  }
})();
