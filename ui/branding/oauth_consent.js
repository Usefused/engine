// Enhance the native form without changing its submitted operation or account identities.
(() => {
  const picker = document.querySelector('.oauth-picker');
  // Management OAuth has no operation picker and must keep its existing form behavior.
  if (!picker) return;
  const search = picker.querySelector('input[type="search"]');
  const cards = Array.from(picker.querySelectorAll('.oauth-service'));
  const count = picker.querySelector('.oauth-selection-count');
  const empty = picker.querySelector('.oauth-empty');
  let searching = false;

  // Count all selected rows, including filtered and collapsed ones, so consent never silently expands or shrinks.
  function updateSelection() {
    let total = 0;
    // Each service keeps its own finite selection summary.
    for (const card of cards) {
      const selected = card.querySelectorAll('input[name="operation"]:checked').length;
      total += selected;
      const badge = card.querySelector('.oauth-badge');
      badge.textContent = selected + ' selected';
      badge.hidden = selected === 0;
      const visible = Array.from(card.querySelectorAll('.oauth-operation')).filter(isVisibleRow);
      const allSelected = visible.length > 0 && visible.every(isSelectedRow);
      const toggle = card.querySelector('.oauth-select-all');
      // Bulk selection always describes the visible finite set, never future operations.
      toggle.textContent = allSelected ? 'Deselect shown (' + visible.length + ')' : 'Select shown (' + visible.length + ')';
      toggle.disabled = visible.length === 0;
    }
    count.textContent = 'Selected operations: ' + total;
  }

  // Search visibility is presentational; hidden checkboxes remain enabled and keep their values.
  function isVisibleRow(row) { return !row.hidden; }

  // The checked input remains the single source of truth for both native POST and summary badges.
  function isSelectedRow(row) { return row.querySelector('input').checked; }

  // Reveal matching operations while remembering the user's disclosure state for when search is cleared.
  function filterOperations() {
    const term = search.value.trim().toLowerCase();
    // Filter service identity and operation names without reconstructing the form.
    for (const card of cards) {
      // Save disclosure state only on entry to search, not on every keystroke.
      if (term && !searching) card.dataset.wasOpen = String(card.open);
      const serviceMatches = card.dataset.service.toLowerCase().includes(term);
      let matches = 0;
      // Hidden rows retain their selected state and remain native form controls.
      for (const row of card.querySelectorAll('.oauth-operation')) {
        row.hidden = !serviceMatches && !row.textContent.toLowerCase().includes(term);
        matches += Number(!row.hidden);
      }
      card.hidden = matches === 0;
      // Search expands matching cards; clearing it restores the original navigation state.
      if (term) card.open = matches > 0;
      else if (searching) card.open = card.dataset.wasOpen === 'true';
    }
    searching = Boolean(term);
    empty.hidden = cards.some(isVisibleRow);
    updateSelection();
  }

  // An explicit bulk action affects only visible operations in this service, retaining every other selection.
  function toggleShown(event) {
    const card = event.currentTarget.closest('.oauth-service');
    const rows = Array.from(card.querySelectorAll('.oauth-operation')).filter(isVisibleRow);
    const checked = !rows.every(isSelectedRow);
    // Update only this explicitly reviewed finite set of visible rows.
    for (const row of rows) row.querySelector('input').checked = checked;
    updateSelection();
  }

  // Progressive controls are hidden until their handlers exist; the plain form remains usable without scripts.
  for (const card of cards) {
    const toggle = card.querySelector('.oauth-select-all');
    toggle.addEventListener('click', toggleShown);
    toggle.hidden = false;
  }
  picker.querySelector('.oauth-search').hidden = false;
  count.hidden = false;
  search.addEventListener('input', filterOperations);
  picker.addEventListener('change', updateSelection);
  updateSelection();
})();
