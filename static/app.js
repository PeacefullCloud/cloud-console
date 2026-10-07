/* Peaceful Cloud Console — minimal progressive enhancement.
   Everything important works without JavaScript; this only adds conveniences. */
(function () {
  "use strict";

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

  // Destructive actions ask for confirmation.
  document.addEventListener("submit", function (event) {
    var form = event.target.closest("form[data-confirm]");
    if (form && !window.confirm(form.getAttribute("data-confirm"))) {
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

  // The job tracker polls itself; stop polling once every job has finished by
  // letting the server render an idle tracker with a slow interval.
  //
  // Live updates via Server-Sent Events (inspect in DevTools under
  // Network > EventStream on GET /jobs/stream). The server keeps one
  // long-lived request open and pushes the same job_list partial only when
  // jobs change, instead of HTMX polling GET /jobs/active every 2s/15s.
  // No EventSource (or a 401/stream error) falls back to that polling.
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
  });
  syncCustomPlan();
})();
