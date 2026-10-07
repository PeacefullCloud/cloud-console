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
        sidebar.classList.toggle("open");
      }
      return;
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
