// init.js — Runs after htmx loads. Handles CSRF token injection, error/toast
// notifications, screen-reader announcements, and post-swap focus.

(function () {
  "use strict";

  // Theme is applied pre-paint by theme-init.js (loaded synchronously in
  // <head>) and toggled/persisted by Alpine in the navbar, so init.js no
  // longer touches it.

  // --- Alpine clipboard components (registered before Alpine starts) ---
  // Command text is read straight from the DOM ([data-cmd]) rather than
  // interpolated from server strings into JS, which keeps the no-injection
  // property and avoids escaping kubectl's quotes/braces.
  function copyToClipboard(text, onCopied) {
    if (!text) return;
    navigator.clipboard.writeText(text).then(function () {
      announce("Copied to clipboard");
      onCopied();
    });
  }
  document.addEventListener("alpine:init", function () {
    // A single command block: copies its own [data-cmd].
    window.Alpine.data("commandBlock", function () {
      return {
        copied: false,
        copy: function () {
          var el = this.$root.querySelector("[data-cmd]");
          if (!el) return;
          var self = this;
          copyToClipboard(el.textContent.trim(), function () {
            self.copied = true;
            setTimeout(function () { self.copied = false; }, 1500);
          });
        },
      };
    });
    // The list wrapper: copies every [data-cmd] beneath it, newline-joined.
    window.Alpine.data("commandList", function () {
      return {
        copied: false,
        copyAll: function () {
          var cmds = Array.prototype.map
            .call(this.$root.querySelectorAll("[data-cmd]"), function (e) {
              return e.textContent.trim();
            })
            .join("\n");
          if (!cmds) return;
          var self = this;
          copyToClipboard(cmds, function () {
            self.copied = true;
            setTimeout(function () { self.copied = false; }, 2000);
          });
        },
      };
    });
  });

  // --- CSRF token injection via htmx:configRequest ---
  // Reads the _csrf cookie and attaches it as X-CSRF-Token header on every
  // mutating htmx request. This replaces the CSP-incompatible js: prefix.
  document.addEventListener("htmx:configRequest", function (evt) {
    var match = document.cookie.match(/_csrf=([^;]+)/);
    if (match) {
      evt.detail.headers["X-CSRF-Token"] = match[1];
    }
  });

  // --- Focus restoration after htmx swaps ---
  // Alpine's MutationObserver initializes swapped-in nodes on its own, so no
  // explicit Alpine.initTree is needed here (and evt.detail.elt is the
  // requesting element, which for outerHTML swaps is already detached).
  document.addEventListener("htmx:afterSwap", function (evt) {
    var elt = evt.detail.elt;
    // Only restore focus for main-content or detail-container swaps
    if (elt && (elt.id === "main-content" || elt.id === "detail-container")) {
      var focusable = elt.querySelector(
        'button:not([disabled]), a[href], input:not([disabled]), [tabindex="0"]'
      );
      if (focusable) {
        focusable.focus();
      }
      announce("Content updated");
    }
  });

  // --- Screen reader announcements ---
  function announce(message) {
    var el = document.getElementById("sr-announcer");
    if (!el) return;
    el.textContent = "";
    requestAnimationFrame(function () {
      el.textContent = message;
    });
  }

  // Expose for use by inline handlers (e.g., copy buttons)
  window.__announce = announce;

  // --- Error toasts ---
  function showToast(msg, cls) {
    var container = document.getElementById("toast-container");
    if (!container) return;
    var el = document.createElement("div");
    el.className = "alert " + cls + " shadow-lg text-sm";
    var span = document.createElement("span");
    span.textContent = msg;
    el.appendChild(span);
    container.appendChild(el);
    setTimeout(function () {
      el.remove();
    }, 5000);
  }

  // Map a toast level to its daisyUI alert class.
  var TOAST_CLASS = {
    error: "alert-error",
    warning: "alert-warning",
    success: "alert-success",
    info: "alert-info",
  };

  // --- Server-driven toasts ---
  // Handlers emit an HX-Trigger header of the form
  //   {"toast":{"level":"error|warning|success|info","message":"..."}}
  // which htmx dispatches as a `toast` event. This is how action failures
  // (e.g. "already acknowledged") and successes surface, since htmx does not
  // swap the bodies of non-2xx responses.
  document.body.addEventListener("toast", function (evt) {
    var d = evt.detail || {};
    if (!d.message) return;
    showToast(d.message, TOAST_CLASS[d.level] || "alert-error");
  });

  document.addEventListener("htmx:responseError", function (evt) {
    var xhr = evt.detail.xhr;
    // Prefer the server's human-readable message over a bare status code.
    var msg = "Request failed (status " + xhr.status + ")";
    try {
      var parsed = JSON.parse(xhr.responseText);
      if (parsed && parsed.error) {
        msg = parsed.error;
      }
    } catch (e) {
      /* non-JSON body — keep the status-code fallback */
    }
    showToast(msg, "alert-error");
  });

  document.addEventListener("htmx:sendError", function () {
    showToast("Network error — check your connection", "alert-warning");
  });
})();
