// SPDX-License-Identifier: AGPL-3.0-or-later

// Transient toast notifications for htmx-driven actions that otherwise give
// no visible feedback: the Load/Unload, tier-change, and reset-password
// controls all use hx-swap="none", so a click does nothing the user can
// see until the next SSE-driven refetch happens to land. A success toast is
// opt-in per control via a data-toast attribute (its value is the message);
// an error toast fires for any failed mutating (non-GET) htmx request,
// since a silent failure is never acceptable. GET requests - sidebar
// navigation and sse.js's own refetch - are ignored either way, so a
// server blip during a live refetch never spams toasts. Plain vanilla JS,
// no framework, per CLAUDE.md Frontend Conventions.
(function () {
  var dismissAfterMs = 4000;

  function showToast(message, kind) {
    var host = document.getElementById("toast-container");
    if (!host || !message) {
      return;
    }
    var toast = document.createElement("div");
    toast.className = kind === "error" ? "toast toast-error" : "toast";
    toast.setAttribute("role", kind === "error" ? "alert" : "status");
    toast.textContent = message;
    host.appendChild(toast);

    // Force a reflow so the .toast-visible transition runs from the initial
    // hidden state instead of being collapsed into a single frame.
    void toast.offsetWidth;
    toast.classList.add("toast-visible");

    window.setTimeout(function () {
      toast.classList.remove("toast-visible");
      var removed = false;
      var drop = function () {
        if (!removed) {
          removed = true;
          toast.remove();
        }
      };
      toast.addEventListener("transitionend", drop);
      // Fallback if transitionend never fires (prefers-reduced-motion, a
      // backgrounded tab).
      window.setTimeout(drop, 400);
    }, dismissAfterMs);
  }

  function isMutating(detail) {
    var rc = detail && detail.requestConfig;
    return !!(rc && rc.verb && rc.verb.toLowerCase() !== "get");
  }

  function toastMessage(elt) {
    if (!elt) {
      return null;
    }
    if (elt.getAttribute && elt.getAttribute("data-toast")) {
      return elt.getAttribute("data-toast");
    }
    var owner = elt.closest ? elt.closest("[data-toast]") : null;
    return owner ? owner.getAttribute("data-toast") : null;
  }

  document.addEventListener("htmx:afterRequest", function (evt) {
    var d = evt.detail || {};
    if (!isMutating(d)) {
      return;
    }
    if (d.successful) {
      var msg = toastMessage(d.elt);
      if (msg) {
        showToast(msg, "success");
      }
      return;
    }
    var status = d.xhr && d.xhr.status ? " (" + d.xhr.status + ")" : "";
    showToast("Something went wrong" + status + ". Please try again.", "error");
  });

  document.addEventListener("htmx:sendError", function (evt) {
    if (isMutating(evt.detail)) {
      showToast("Network error - could not reach the server.", "error");
    }
  });

  document.addEventListener("htmx:timeout", function (evt) {
    if (isMutating(evt.detail)) {
      showToast("The request timed out. Please try again.", "error");
    }
  });
})();
