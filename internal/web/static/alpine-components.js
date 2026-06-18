// Alpine.data clipboard components for the "Verify the Diagnosis" command list.
//
// Loaded BEFORE alpine core (like the focus plugin) so the alpine:init listener
// is registered before Alpine dispatches alpine:init / runs Alpine.start().
// Registering this from a script that loads after alpine core would be too late
// — the event fires in the microtask right after the core script — and the
// components would be undefined, silently breaking the copy buttons.
//
// Command text is read straight from the DOM ([data-cmd]) rather than
// interpolated from server strings into JS, which preserves the no-injection
// property and avoids escaping kubectl's quotes/braces.
(function () {
  "use strict";

  function copyToClipboard(text, onCopied) {
    if (!text) return;
    navigator.clipboard.writeText(text).then(function () {
      if (window.__announce) window.__announce("Copied to clipboard");
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
            setTimeout(function () {
              self.copied = false;
            }, 1500);
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
            setTimeout(function () {
              self.copied = false;
            }, 2000);
          });
        },
      };
    });
  });
})();
