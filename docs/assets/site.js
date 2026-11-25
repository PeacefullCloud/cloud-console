(function () {
  // Mobile navigation toggle
  var toggle = document.getElementById("nav-toggle");
  var nav = document.getElementById("site-nav");
  if (toggle && nav) {
    toggle.addEventListener("click", function () {
      var open = nav.classList.toggle("open");
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
    });
  }

  // Copy buttons for <pre> blocks
  document.querySelectorAll("pre").forEach(function (pre) {
    var wrap = pre.parentElement;
    if (!wrap || !wrap.classList.contains("code-wrap")) return;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy-btn";
    btn.textContent = "Copy";
    btn.addEventListener("click", function () {
      var text = pre.innerText;
      if (!navigator.clipboard) return;
      navigator.clipboard.writeText(text).then(function () {
        btn.textContent = "Copied";
        setTimeout(function () { btn.textContent = "Copy"; }, 1600);
      });
    });
    wrap.appendChild(btn);
  });

  // Screenshot lightbox
  var modal = document.getElementById("lightbox");
  var modalImg = document.getElementById("lightbox-img");
  var modalCap = document.getElementById("lightbox-caption");
  var modalClose = document.getElementById("lightbox-close");

  if (modal && modalImg && modalCap && modalClose) {
    document.querySelectorAll(".shot-frame").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var img = btn.querySelector("img");
        if (!img) return;
        modalImg.src = img.currentSrc || img.src;
        modalImg.alt = img.alt;
        modalCap.textContent = btn.getAttribute("data-caption") || img.alt;
        modal.classList.add("open");
        modalClose.focus();
      });
    });

    function closeModal() {
      modal.classList.remove("open");
      modalImg.removeAttribute("src");
    }

    modalClose.addEventListener("click", closeModal);
    modal.addEventListener("click", function (e) {
      if (e.target === modal) closeModal();
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && modal.classList.contains("open")) closeModal();
    });
  }
})();
