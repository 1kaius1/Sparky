// SPDX-License-Identifier: AGPL-3.0-or-later

// "Select all" for the scan-results tables on the Inventory scan page
// (web/templates/partials/scan_results.html): a checkbox in a table's header
// (marked data-select-all) ticks or clears every row checkbox in that same
// table, and shows a mixed (indeterminate) state when only some are ticked.
//
// Rows whose import is blocked render a disabled checkbox and are never
// touched - a disabled box is not submitted, but ticking it visually would
// still mislead. The header box has no name, so it is never submitted either.
// The results partial is replaced wholesale on every poll and re-render
// (outerHTML swap), so nothing is stored: one delegated listener on the
// document, state recomputed from the DOM on each change.
// Plain vanilla JS, no framework, per CLAUDE.md Frontend Conventions.
(function () {
  function rowBoxes(table) {
    return table.querySelectorAll('input[type="checkbox"][name^="sel_"]:not(:disabled)');
  }

  function syncHeader(table) {
    var head = table.querySelector("input[data-select-all]");
    if (!head) {
      return;
    }
    var boxes = rowBoxes(table);
    var ticked = 0;
    for (var i = 0; i < boxes.length; i++) {
      if (boxes[i].checked) {
        ticked++;
      }
    }
    head.checked = boxes.length > 0 && ticked === boxes.length;
    head.indeterminate = ticked > 0 && ticked < boxes.length;
  }

  document.addEventListener("change", function (ev) {
    var t = ev.target;
    if (!t || t.type !== "checkbox") {
      return;
    }
    var table = t.closest ? t.closest("table") : null;
    if (!table) {
      return;
    }
    if (t.hasAttribute("data-select-all")) {
      var boxes = rowBoxes(table);
      for (var i = 0; i < boxes.length; i++) {
        boxes[i].checked = t.checked;
      }
      t.indeterminate = false;
      return;
    }
    if (t.name && t.name.indexOf("sel_") === 0) {
      syncHeader(table);
    }
  });
})();
