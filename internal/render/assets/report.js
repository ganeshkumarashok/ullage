// ullage report — progressive enhancement only.
//
// The document is complete and readable with this file removed: the waterfall
// is server-rendered SVG, evidence sits in native <details>, and every command
// is already selectable text. This adds a copy button and nothing that the
// report depends on. There is no fetch, no storage, no telemetry.
(function () {
  "use strict";

  if (!navigator.clipboard && !document.queryCommandSupported) return;

  function flash(button, text) {
    var original = button.getAttribute("data-label") || button.textContent;
    button.textContent = text;
    button.disabled = true;
    setTimeout(function () {
      button.textContent = original;
      button.disabled = false;
    }, 1200);
  }

  function copy(text, button) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(
        function () { flash(button, "copied"); },
        function () { flash(button, "press ⌘C"); }
      );
      return;
    }
    // file:// documents in older browsers have no async clipboard.
    var area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    try {
      flash(button, document.execCommand("copy") ? "copied" : "press ⌘C");
    } catch (e) {
      flash(button, "press ⌘C");
    }
    document.body.removeChild(area);
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest ? event.target.closest("button.copy") : null;
    if (!button) return;
    var pre = document.getElementById(button.getAttribute("data-for"));
    if (!pre) return;
    event.preventDefault();
    copy(pre.textContent.trim(), button);
  });

  // Buttons are inert without a handler, so they are only revealed once one
  // is attached. A button that does nothing is worse than no button.
  var buttons = document.querySelectorAll("button.copy");
  for (var i = 0; i < buttons.length; i++) {
    buttons[i].hidden = false;
    buttons[i].setAttribute("data-label", buttons[i].textContent);
  }
})();
