/** Indents the active selection while keeping line-boundary selections and caret positions stable. */
export function indentCode(value: string, start: number, end: number, outdent: boolean) {
  // A single-line Tab replaces the selection with one soft tab, matching the editor's two-space display.
  if (!outdent && !value.slice(start, end).includes("\n")) {
    return { value: value.slice(0, start) + "  " + value.slice(end), start: start + 2, end: start + 2 };
  }
  const first = value.slice(0, start).lastIndexOf("\n") + 1;
  // An endpoint at the next line's beginning must not indent an unselected line.
  const endpoint = end > start && value[end - 1] === "\n" ? end - 1 : end;
  const newline = value.indexOf("\n", endpoint);
  // Include the full final line so outdent also works with a caret before its indentation.
  const last = newline === -1 ? value.length : newline;
  const lines = value.slice(first, last).split("\n");
  let offset = first;
  let nextStart = start;
  let nextEnd = end;
  // Each prefix edit maps the original selection forward without selecting extra source text.
  const edited = lines.map((line) => {
    // Only leading whitespace is removable; untouched lines keep their contents.
    const removed = outdent ? (line.match(/^(\t| {1,2})/)?.[0].length ?? 0) : 0;
    const added = outdent ? 0 : 2;
    nextStart += added * Number(offset <= start) - Math.min(removed, Math.max(0, start - offset));
    nextEnd += added - Math.min(removed, Math.max(0, end - offset));
    offset += line.length + 1;
    // Outdent removes at most one indentation level, including pasted hard tabs.
    return outdent ? line.slice(removed) : "  " + line;
  });
  return { value: value.slice(0, first) + edited.join("\n") + value.slice(last), start: nextStart, end: nextEnd };
}
