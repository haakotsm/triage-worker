// init.js — Runs after htmx loads. Handles theme restoration, CSRF token
// injection, error toast notifications, and Alpine/htmx interop.

(function () {
  "use strict";

  // --- Theme ---
  // Restore saved theme immediately to prevent FOUC.
  var saved = localStorage.getItem("theme") || "luxury";
  document.documentElement.setAttribute("data-theme", saved);

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
