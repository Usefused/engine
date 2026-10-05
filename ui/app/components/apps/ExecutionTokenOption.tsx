/** Makes initial token issuance an explicit creation choice shared by every app adapter. */
export function ExecutionTokenOption({ checked, disabled, onChange }: { checked: boolean; disabled?: boolean; onChange: (checked: boolean) => void }) {
  return <label className="flex items-start gap-3 rounded-lg border border-slate-200 bg-slate-50 p-3">
    <input type="checkbox" checked={checked} disabled={disabled} onChange={(event) => onChange(event.target.checked)} className="mt-0.5 h-4 w-4 shrink-0 accent-[var(--brand-violet)]" />
    <span><span className="block text-sm font-medium text-slate-900">Generate execution token</span><span className="mt-1 block text-xs font-normal text-slate-500">Create a token to call this app. You can create one later instead.</span></span>
  </label>;
}
