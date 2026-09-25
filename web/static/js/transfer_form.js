// SPDX-License-Identifier: AGPL-3.0-or-later

// Behavior for the model transfer form (web/templates/pages/initiate_transfer.html):
// switch between the Internet and Another node fieldsets, and keep the
// submit control disabled in peer mode until "Check Destination" has passed.
// The gate is a convenience, not a security boundary - the server never
// trusts it (a passed check proves only that a TCP path was open at check
// time). A change to anything the check depended on invalidates it.
// Plain vanilla JS, no framework, per CLAUDE.md Frontend Conventions.
(function () {
  function form() {
    return document.getElementById("transfer-form");
  }

  function isPeer(f) {
    var sel = f.querySelector("#source_type");
    return !!sel && sel.value === "peer_node";
  }

  // A disabled fieldset is excluded from submission, so the inactive mode's
  // fields are never sent - hiding alone would still submit them.
  function setFieldset(f, id, active) {
    var fs = f.querySelector("#" + id);
    if (!fs) {
      return;
    }
    fs.hidden = !active;
    fs.disabled = !active;
  }

  function apply() {
    var f = form();
    if (!f) {
      return;
    }
    var peer = isPeer(f);
    setFieldset(f, "internet-fields", !peer);
    setFieldset(f, "peer-fields", peer);
    var submit = f.querySelector("#transfer-submit");
    if (submit) {
      var blocked = peer && f.getAttribute("data-check-passed") !== "true";
      submit.disabled = blocked;
      submit.title = blocked ? "Run Check Destination first" : "";
    }
  }

  function invalidateCheck(f) {
    f.removeAttribute("data-check-passed");
    var status = f.querySelector("#check-status");
    if (status) {
      status.innerHTML = "";
    }
  }

  var checkedFields = { dest_node_id: true, source_node_id: true, source_interface: true, entry: true };

  document.addEventListener("change", function (e) {
    var f = form();
    if (!f || !f.contains(e.target)) {
      return;
    }
    var key = e.target.name || e.target.id;
    if (checkedFields[key]) {
      invalidateCheck(f);
    }
    apply();
  });

  // Any swap may have delivered a check result or new peer options.
  document.addEventListener("htmx:afterSwap", function (e) {
    var f = form();
    if (!f) {
      return;
    }
    // Reloaded peer options (source node changed, or a Rescan finished)
    // mean the previous check no longer describes what is selected.
    if (e.detail && e.detail.target && e.detail.target.id === "peer-options") {
      invalidateCheck(f);
    }
    var result = f.querySelector("#check-status [data-check-state]");
    if (result && result.getAttribute("data-check-state") === "passed") {
      f.setAttribute("data-check-passed", "true");
    } else {
      f.removeAttribute("data-check-passed");
    }
    apply();
  });

  // After a Rescan request, give the agent a moment to report, then reload
  // the interface list. The check no longer applies to a changed list.
  document.addEventListener("htmx:afterRequest", function (e) {
    var elt = e.detail && e.detail.elt;
    if (!elt || !elt.hasAttribute || !elt.hasAttribute("data-rescan")) {
      return;
    }
    window.setTimeout(function () {
      var target = document.getElementById("peer-options");
      if (target && window.htmx) {
        window.htmx.trigger(target, "refresh");
      }
    }, 1500);
  });

  document.addEventListener("DOMContentLoaded", apply);
  document.addEventListener("htmx:load", apply);
})();
