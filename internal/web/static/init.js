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

  // --- Alpine re-initialization after htmx swaps ---
  // Alpine's MutationObserver handles most cases, but explicit initTree
  // ensures deterministic initialization for outerHTML swaps.
  document.addEventListener("htmx:afterSwap", function (evt) {
    var elt = evt.detail.elt;
    if (window.Alpine && elt) {
      Alpine.initTree(elt);
    }
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

  document.addEventListener("htmx:responseError", function (evt) {
    showToast(
      "Request failed (status " + evt.detail.xhr.status + ")",
      "alert-error"
    );
  });

  document.addEventListener("htmx:sendError", function () {
    showToast("Network error — check your connection", "alert-warning");
  });
})();
