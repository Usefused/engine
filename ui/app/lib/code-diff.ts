export type CodeDiffLine = { kind: "unchanged" | "added" | "removed"; text: string; oldLine?: number; newLine?: number };

/** Compares logical lines with bounded work so large pasted files cannot stall the editor. */
export function diffCode(before: string, after: string): CodeDiffLine[] {
  // Empty files have no source lines, while trailing newlines remain meaningful changes.
  const oldLines = before === "" ? [] : before.split("\n"), newLines = after === "" ? [] : after.split("\n");
  let start = 0, end = 0;
  // Trim common edges before allocating the comparison matrix, especially for small edits in large files.
  while (start < oldLines.length && start < newLines.length && oldLines[start] === newLines[start]) start++;
  while (end < oldLines.length - start && end < newLines.length - start && oldLines[oldLines.length - 1 - end] === newLines[newLines.length - 1 - end]) end++;
  const oldEnd = oldLines.length - end, newEnd = newLines.length - end;
  const n = oldEnd - start, m = newEnd - start;
  const rows: CodeDiffLine[] = [];
  /** Emits paired line numbers only for content shared by both versions. */
  function unchanged(oldIndex: number, newIndex: number) { rows.push({ kind: "unchanged", text: oldLines[oldIndex], oldLine: oldIndex + 1, newLine: newIndex + 1 }); }
  /** Keeps removed text outside the editable value so it can never reach compilation. */
  function removed(index: number) { rows.push({ kind: "removed", text: oldLines[index], oldLine: index + 1 }); }
  /** Records current line positions for highlighting the native editing surface. */
  function added(index: number) { rows.push({ kind: "added", text: newLines[index], newLine: index + 1 }); }
  for (let index = 0; index < start; index++) unchanged(index, index);
  // Widely replaced files use one honest replacement block instead of an unbounded quadratic comparison.
  if (n * m > 500000) {
    for (let index = start; index < oldEnd; index++) removed(index);
    for (let index = start; index < newEnd; index++) added(index);
  } else {
    const width = m + 1, matrix = new Uint32Array((n + 1) * width);
    // Longest common subsequences keep nearby insertions and deletions aligned, including repeated lines.
    for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) {
      matrix[i * width + j] = oldLines[start + i] === newLines[start + j]
        ? 1 + matrix[(i + 1) * width + j + 1]
        : Math.max(matrix[(i + 1) * width + j], matrix[i * width + j + 1]);
    }
    let i = 0, j = 0;
    // Prefer removals on a tie so replacements read old text first, like a unified Git diff.
    while (i < n || j < m) {
      if (i < n && j < m && oldLines[start + i] === newLines[start + j]) unchanged(start + i++, start + j++);
      else if (i < n && (j === m || matrix[(i + 1) * width + j] >= matrix[i * width + j + 1])) removed(start + i++);
      else added(start + j++);
    }
  }
  for (let index = 0; index < end; index++) unchanged(oldEnd + index, newEnd + index);
  return rows;
}

/** Retains two surrounding lines per change while collapsing distant unchanged code. */
export function codeDiffContext(rows: CodeDiffLine[]): Array<CodeDiffLine | { kind: "gap"; count: number }> {
  const visible = new Set<number>();
  rows.forEach((row, index) => {
    // Unchanged runs only appear when they help locate a nearby edit.
    if (row.kind !== "unchanged") for (let at = Math.max(0, index - 2); at <= Math.min(rows.length - 1, index + 2); at++) visible.add(at);
  });
  const result: Array<CodeDiffLine | { kind: "gap"; count: number }> = [];
  let skipped = 0;
  rows.forEach((row, index) => {
    // Adjacent hidden lines share one accessible context marker.
    if (!visible.has(index)) { skipped++; return; }
    if (skipped) { result.push({ kind: "gap", count: skipped }); skipped = 0; }
    result.push(row);
  });
  // Preserve trailing context information rather than implying the file ends at the last change.
  if (skipped) result.push({ kind: "gap", count: skipped });
  return result;
}
