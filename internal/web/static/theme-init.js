// Loaded synchronously in <head> (no defer) so it runs before first paint and
// prevents a flash of the default theme (FOUC) for users who chose light mode.
// It only SETS the saved theme; Alpine (in the navbar) owns the toggle and
// persistence, so theme ownership lives in exactly two clear places.
(function () {
  try {
    var t = localStorage.getItem("theme");
    if (t) {
      document.documentElement.setAttribute("data-theme", t);
    }
  } catch (e) {
    /* localStorage unavailable — keep the server-rendered default theme */
  }
})();
