/** Keeps draft dialogs within workspace context; standalone components retain their normal document portal. */
export function workspaceDialogHost(): HTMLElement {
  // Public pages and component previews may not mount the authenticated workspace shell.
  return document.getElementById("fused-workspace-dialogs") ?? document.body;
}
