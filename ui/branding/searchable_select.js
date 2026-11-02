let nextSelectId = 0;

/** Enhance one native select for React and server-rendered forms without changing submitted values. */
export function enhanceSearchableSelect(select) {
  // Native multi-select/listbox semantics are intentionally preserved rather than reduced to one choice.
  if (select.multiple || select.size > 1) return { sync() {}, destroy() {} };
  const document = select.ownerDocument;
  const view = document.defaultView;
  const originalHidden = select.hidden;
  const originalFocus = select.focus;
  const originalError = select.validity.customError ? select.validationMessage : '';
  const id = (select.id || 'fused-select-' + ++nextSelectId) + '-search';
  const wrapper = document.createElement('span');
  wrapper.className = 'fused-searchable-select';
  const input = document.createElement('input');
  input.id = id;
  input.type = 'text';
  input.autocomplete = 'off';
  input.spellcheck = false;
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-controls', id + '-options');
  const menu = document.createElement('div');
  menu.className = 'fused-select-menu';
  menu.hidden = true;
  const list = document.createElement('ul');
  list.id = id + '-options';
  list.setAttribute('role', 'listbox');
  const empty = document.createElement('p');
  empty.className = 'fused-select-empty';
  empty.setAttribute('role', 'status');
  empty.textContent = 'No matching options.';
  menu.append(list, empty);
  wrapper.append(input, menu);
  select.after(wrapper);
  const labels = Array.from(select.labels || []).map(label => ({ label, htmlFor: label.getAttribute('for') }));
  // Existing labels must focus the visible combobox while the hidden select keeps its name and UUID value.
  for (const { label } of labels) label.htmlFor = input.id;
  select.hidden = true;
  // Preserve callers' native refs, including their imperative focus behavior.
  select.focus = options => input.focus(options);
  let matches = [];
  let active = -1;
  let editing = false;
  let destroyed = false;

  /** Disabled options and disabled groups can be displayed but never selected. */
  function enabled(option) {
    return !option.disabled && !(option.parentElement.tagName === 'OPTGROUP' && option.parentElement.disabled) && (!select.required || option.value !== '');
  }

  /** Dismiss suggestions and their accessibility reference without changing the selection. */
  function close() {
    menu.hidden = true;
    input.setAttribute('aria-expanded', 'false');
    input.removeAttribute('aria-activedescendant');
  }

  /** Keep the active suggestion visible without moving keyboard focus away from the search field. */
  function highlight(index) {
    active = index;
    Array.from(list.children).forEach((row, current) => row.setAttribute('aria-selected', String(current === active)));
    const row = list.children[active];
    // Empty results and disabled-only results have no active descendant.
    if (!row) { input.removeAttribute('aria-activedescendant'); return; }
    input.setAttribute('aria-activedescendant', row.id);
    row.scrollIntoView({ block: 'nearest' });
  }

  /** Commit only a currently enabled native option, then notify the existing native/React change handler. */
  function choose(option) {
    // Disabled controls and removed options cannot be committed through a stale suggestion.
    if (select.disabled || !select.contains(option) || !enabled(option)) return;
    select.value = option.value;
    select.setCustomValidity(originalError);
    editing = false;
    input.setCustomValidity(originalError);
    close();
    select.dispatchEvent(new view.Event('change', { bubbles: true }));
    sync();
  }

  /** Filter the latest native options so changed React children cannot leave stale routing choices. */
  function show(filtered) {
    // Disabled native controls have no interactive enhancement.
    if (select.disabled) return;
    const term = filtered ? input.value.trim().toLocaleLowerCase() : '';
    matches = Array.from(select.options).filter(option => (!select.required || option.value !== '') && option.text.toLocaleLowerCase().includes(term));
    list.replaceChildren();
    matches.forEach((option, index) => {
      const row = document.createElement('li');
      row.id = id + '-option-' + index;
      row.setAttribute('role', 'option');
      row.setAttribute('aria-disabled', String(!enabled(option)));
      row.textContent = option.text;
      row.addEventListener('mousedown', event => event.preventDefault());
      row.addEventListener('click', () => choose(option));
      list.append(row);
    });
    empty.hidden = matches.length !== 0;
    menu.hidden = false;
    input.setAttribute('aria-expanded', 'true');
    highlight(matches.findIndex(option => enabled(option) && option.value === select.value));
  }

  /** Typed text filters options; it never becomes an arbitrary submitted value. */
  function search() {
    editing = true;
    select.selectedIndex = -1;
    const message = input.value || select.required ? 'Choose an option from the list.' : originalError;
    select.setCustomValidity(message);
    input.setCustomValidity(message);
    show(true);
  }

  /** Handle selection keys without implicitly submitting the surrounding app or consent form. */
  function navigate(event) {
    // Escape closes the menu without accepting partial text.
    if (event.key === 'Escape') { event.preventDefault(); close(); return; }
    // Enter belongs to this control even when the menu is closed; consent requires its explicit button.
    if (event.key === 'Enter') {
      event.preventDefault();
      // An open, active suggestion is the only keyboard commit target.
      if (!menu.hidden && active >= 0) choose(matches[active]);
      else show(editing);
      return;
    }
    // Home/End retain normal text editing unless the suggestions are already open.
    if (!['ArrowDown', 'ArrowUp'].includes(event.key) && (menu.hidden || !['Home', 'End'].includes(event.key))) return;
    event.preventDefault();
    // Reopening an established selection shows all options instead of filtering by its label.
    if (menu.hidden) show(editing);
    const indices = matches.flatMap((option, index) => enabled(option) ? [index] : []);
    // Avoid wraparound arithmetic when every result is disabled or missing.
    if (!indices.length) { highlight(-1); return; }
    const position = indices.indexOf(active);
    const forward = event.key === 'ArrowDown';
    const index = event.key === 'Home' ? 0 : event.key === 'End' ? indices.length - 1 : position < 0 ? (forward ? 0 : indices.length - 1) : (position + (forward ? 1 : -1) + indices.length) % indices.length;
    highlight(indices[index]);
  }

  /** Mirror controlled values, accessibility, disabled state and presentation after React commits. */
  function sync() {
    input.className = select.className;
    input.dataset.density = select.dataset.density || 'regular';
    input.dataset.tone = select.dataset.tone || 'surface';
    input.placeholder = select.dataset.searchPlaceholder || 'Search options…';
    input.disabled = select.disabled;
    input.required = select.required;
    // Caller-authored accessible names and descriptions remain attached to the visible control.
    for (const attribute of ['aria-label', 'aria-labelledby', 'aria-describedby', 'aria-invalid']) {
      const value = select.getAttribute(attribute);
      if (value === null) input.removeAttribute(attribute);
      else input.setAttribute(attribute, value);
    }
    // Empty required placeholders remain empty so native validation cannot mistake their label for a selection.
    input.value = select.required && select.value === '' ? '' : select.selectedOptions[0]?.text || '';
    // A new controlled selection supersedes a previously incomplete search.
    if (select.selectedIndex >= 0) select.setCustomValidity(originalError);
    input.setCustomValidity(select.validity.customError ? select.validationMessage : '');
    editing = false;
    close();
  }

  /** Open the current authorized choices while preserving any in-progress filter. */
  function open() { show(editing); }
  /** Route hidden-native validation feedback to the visible field without bypassing form validity. */
  function invalid(event) { event.preventDefault(); input.focus(); input.reportValidity(); }
  /** Wait for the browser to restore native defaults before reflecting a form reset. */
  function reset() { view.queueMicrotask(() => { if (!destroyed) { select.setCustomValidity(originalError); sync(); } }); }
  input.addEventListener('focus', open);
  input.addEventListener('click', open);
  input.addEventListener('input', search);
  input.addEventListener('keydown', navigate);
  input.addEventListener('blur', close);
  select.addEventListener('change', sync);
  select.addEventListener('invalid', invalid);
  const form = select.form;
  form?.addEventListener('reset', reset);
  sync();

  /** Remove only this enhancement so React unmounts, StrictMode and opt-out restore native behavior. */
  function destroy() {
    destroyed = true;
    select.removeEventListener('change', sync);
    select.removeEventListener('invalid', invalid);
    form?.removeEventListener('reset', reset);
    select.hidden = originalHidden;
    select.focus = originalFocus;
    select.setCustomValidity(originalError);
    // Restore explicit and implicit labels exactly as the caller authored them.
    for (const { label, htmlFor } of labels) {
      if (htmlFor === null) label.removeAttribute('for');
      else label.setAttribute('for', htmlFor);
    }
    wrapper.remove();
  }
  return { sync, destroy };
}
