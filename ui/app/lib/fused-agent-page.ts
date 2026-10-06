/** Restricts automatic context to the active page while allowing explicit value privacy on any ancestor. */
export function fusedValueHidden(element: Element): boolean {
  // A descendant cannot override a private container; password values are never automatically disclosed.
  if (element.closest('[data-fused-visible="false"]') || element.matches('input[type="password"], input[type="hidden"]')) return true;
  const identity = [element.getAttribute("name"), element.id, element.getAttribute("autocomplete"), element.getAttribute("aria-label")].join(" ");
  return /(?:^|\s)(?:token|secret|authorization)(?:\s|$)|password|(?:api|private|signing|encryption)[-_ ]?key|(?:access|refresh|execution|bearer)[-_ ]?token|secret[-_ ]?value|credential[-_ ]?value/i.test(identity);
}

/** Keeps navigation context useful without disclosing OAuth codes or URL-carried credentials. */
export function fusedPagePath(location: Pick<Location, "href">): string {
  const url = new URL(location.href);
  for (const key of [...url.searchParams.keys()]) {
    // Authentication query values are not page evidence, even when the browser URL contains them.
    if (/^(?:code|password|secret|api[_-]?key|(?:access|refresh|id|execution)[_-]?token)$/i.test(key)) url.searchParams.delete(key);
  }
  return url.pathname + url.search;
}

type Field = HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement;
export interface AgentField { id: string; label: string; type: string; value?: string | boolean; private: boolean; editable: boolean; options?: { value: string; label: string }[] }
export interface PageSnapshot { path: string; revision: number; text: string; fields: AgentField[]; links: { label: string; path: string }[] }

/** Maintains opaque field handles so model output cannot select arbitrary DOM nodes or run script. */
export class FusedAgentPage {
  private fields = new Map<string, Field>();
  private ids = new WeakMap<Element, string>();
  private sequence = 0;
  private revision = 0;
  private fingerprint = "";

  /** Returns visible non-sensitive page content and editable field metadata, never HTML or hidden values. */
  snapshot(root: HTMLElement, location: Location): PageSnapshot {
    const fields: AgentField[] = [];
    this.fields.clear();
    for (const field of root.querySelectorAll<Field>("input, textarea, select")) {
      // Agent controls and non-rendered fields are outside the connected website form.
      if (!this.visible(field) || field.closest("[data-fused-agent]") || field.matches('input[type="submit"], input[type="button"], input[type="file"]')) continue;
      let id = this.ids.get(field);
      // IDs remain stable only for the lifetime of a real field in this page.
      if (!id) { id = `field-${++this.sequence}`; this.ids.set(field, id); }
      this.fields.set(id, field);
      const hidden = fusedValueHidden(field);
      const label = Array.from(field.labels ?? []).map((label) => this.pageText(label).trim()).join(" ") || field.getAttribute("aria-label") || field.name || "Unlabelled field";
      // Native :disabled includes inherited fieldset locks while an existing form action is running.
      const item: AgentField = { id, label: label.slice(0, 240), type: field.type, private: hidden, editable: !hidden && !field.closest('[data-fused-editable="false"]') && !field.matches(":disabled") && !("readOnly" in field && field.readOnly) };
      // Protected fields expose their purpose, but never their value, checked state, or choices.
      if (!hidden) {
        item.value = field instanceof HTMLInputElement && ["checkbox", "radio"].includes(field.type) ? field.checked : field.value;
        // Only render-backed choices may be selected by the agent.
        if (field instanceof HTMLSelectElement) item.options = Array.from(field.options).filter((option) => !fusedValueHidden(option) && !option.disabled).map((option) => ({ value: option.value, label: option.label }));
      }
      fields.push(item);
    }
    const text = this.pageText(root).slice(0, 24000);
    const links = Array.from(root.querySelectorAll<HTMLAnchorElement>("a[href]")).filter((link) => this.visible(link) && !fusedValueHidden(link) && !link.closest("[data-fused-agent]") && link.origin === location.origin).map((link) => ({ label: this.pageText(link).trim().slice(0, 120), path: fusedPagePath(link) })).filter((link) => link.path.startsWith("/integrations"));
    const path = fusedPagePath(location);
    const fingerprint = JSON.stringify({ path, text, fields, links });
    // Any visible change invalidates writes based on an older snapshot.
    if (fingerprint !== this.fingerprint) { this.fingerprint = fingerprint; this.revision++; }
    return { path, revision: this.revision, text, fields, links };
  }

  /** Updates one real form control through native events, preserving React's validation and submission flow. */
  update(root: HTMLElement, location: Location, revision: number, id: string, value: string | boolean): PageSnapshot {
    const before = this.snapshot(root, location);
    // Re-read before every write so navigation, user typing, and privacy changes invalidate stale proposals.
    if (before.revision !== revision) throw new Error("The page changed. Read its current fields before editing.");
    const field = this.fields.get(id);
    const metadata = before.fields.find((item) => item.id === id);
    // Hidden values are user-owned; model tools cannot fill, reveal, or overwrite them.
    if (!field || !metadata?.editable || fusedValueHidden(field)) throw new Error("This field is unavailable or private. Ask the user to edit it.");
    const checked = field instanceof HTMLInputElement && ["checkbox", "radio"].includes(field.type);
    // Accept only the control's native value type and declared choices.
    if (checked ? typeof value !== "boolean" : typeof value !== "string") throw new Error("The value does not match this field type.");
    // Invalid select choices must not turn into a silent empty value.
    if (field instanceof HTMLSelectElement && !Array.from(field.options).some((option) => option.value === value && !option.disabled && !fusedValueHidden(option))) throw new Error("Choose one of this field's available options.");
    // React implements checked controls through click events; native click preserves its controlled-state handler.
    if (checked) {
      if ((field as HTMLInputElement).checked !== value) (field as HTMLInputElement).click();
      return this.snapshot(root, location);
    }
    const prototype = field instanceof HTMLSelectElement ? HTMLSelectElement.prototype : field instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    const property = "value";
    Object.getOwnPropertyDescriptor(prototype, property)?.set?.call(field, value);
    field.dispatchEvent(new Event("input", { bubbles: true }));
    field.dispatchEvent(new Event("change", { bubbles: true }));
    return this.snapshot(root, location);
  }

  /** Skips hidden UI, including responsive duplicates, rather than inferring what the user can see. */
  private visible(element: Element): boolean {
    const style = getComputedStyle(element);
    return element.getClientRects().length > 0 && style.visibility !== "hidden" && style.visibility !== "collapse" && !element.closest('[hidden], [aria-hidden="true"]');
  }

  /** Reads only rendered text nodes whose entire ancestry permits disclosure. */
  private pageText(root: HTMLElement): string {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    const parts: string[] = [];
    let node: Node | null;
    while ((node = walker.nextNode())) {
      const parent = node.parentElement;
      // Values in controls are handled above; private wrappers also protect non-input text such as tokens.
      if (!parent || !this.visible(parent) || fusedValueHidden(parent) || parent.closest("[data-fused-agent],script,style,input,textarea,select")) continue;
      const text = node.textContent?.trim();
      // Empty formatting nodes provide no page evidence.
      if (text) parts.push(text);
    }
    return parts.join("\n");
  }
}
