import { isValidElement, useEffect, useRef, useState, type ComponentPropsWithoutRef, type ReactNode } from 'react';
import { Check, Copy } from 'lucide-react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import SyntaxHighlighter from 'react-syntax-highlighter/dist/esm/prism-light.js';
import typescript from 'react-syntax-highlighter/dist/esm/languages/prism/typescript.js';
import javascript from 'react-syntax-highlighter/dist/esm/languages/prism/javascript.js';
import json from 'react-syntax-highlighter/dist/esm/languages/prism/json.js';
import yaml from 'react-syntax-highlighter/dist/esm/languages/prism/yaml.js';
import bash from 'react-syntax-highlighter/dist/esm/languages/prism/bash.js';
import python from 'react-syntax-highlighter/dist/esm/languages/prism/python.js';

// Limit the bundle to languages used in app source, configuration, and CLI examples.
for (const [name, grammar] of Object.entries({ typescript, javascript, json, yaml, bash, python })) SyntaxHighlighter.registerLanguage(name, grammar);
const aliases: Record<string, string> = { ts: 'typescript', js: 'javascript', yml: 'yaml', sh: 'bash', shell: 'bash', py: 'python' };
const supported = new Set(['typescript', 'javascript', 'json', 'yaml', 'bash', 'python']);
const tokens = {
  comment: { color: '#64748b' }, keyword: { color: '#7c3aed' }, string: { color: '#047857' },
  number: { color: '#b45309' }, boolean: { color: '#b45309' }, function: { color: '#2563eb' },
  'class-name': { color: '#0e7490' }, operator: { color: '#475569' }, punctuation: { color: '#64748b' },
};

/** Shows source as selectable text; copying never executes code or applies it to a workspace form. */
function CodeBlock({ children }: ComponentPropsWithoutRef<'pre'>) {
  const [copyState, setCopyState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const timer = useRef<ReturnType<typeof setTimeout>>();
  // Markdown emits one code child for both fenced and indented blocks, including unfinished streamed fences.
  const child = isValidElement<{ className?: string; children?: ReactNode }>(children) ? children : null;
  const code = typeof child?.props.children === 'string' ? child.props.children : '';
  const declared = /language-([^\s]+)/.exec(child?.props.className ?? '')?.[1]?.toLowerCase() ?? '';
  const language = aliases[declared] ?? declared;
  // Only registered grammars reach the highlighter; arbitrary language labels remain plain text.
  const highlight = supported.has(language);
  // A copy acknowledgement belongs to the exact displayed source, not subsequent streaming revisions.
  useEffect(() => { setCopyState('idle'); return () => clearTimeout(timer.current); }, [code]);

  /** Reports clipboard failures without claiming success or altering the user's selected source. */
  async function copyCode() {
    clearTimeout(timer.current);
    try { await navigator.clipboard.writeText(code); setCopyState('copied'); }
    catch { setCopyState('failed'); }
    timer.current = setTimeout(() => setCopyState('idle'), 2000);
  }

  // Unexpected Markdown children retain their text rather than disappearing or being interpreted as HTML.
  if (!child || typeof child.props.children !== 'string') return <pre className="max-w-full overflow-x-auto whitespace-pre p-3">{children}</pre>;
  return <div className="my-3 min-w-0 max-w-full overflow-hidden rounded-xl border border-slate-200 bg-white">
    <div className="flex items-center justify-between gap-3 border-b border-slate-200 bg-slate-50 px-3 py-2">
      <span className="min-w-0 truncate font-mono text-[11px] font-medium text-slate-500">{declared || 'code'}</span>
      <button type="button" onClick={copyCode} aria-label="Copy code" className="inline-flex shrink-0 items-center gap-1.5 rounded px-1.5 py-1 text-xs text-slate-600 hover:bg-slate-200 focus-visible:outline focus-visible:outline-2 focus-visible:outline-violet-700">
        {copyState === 'copied' ? <Check aria-hidden="true" className="h-3.5 w-3.5" /> : <Copy aria-hidden="true" className="h-3.5 w-3.5" />}<span aria-live="polite">{copyState === 'copied' ? 'Copied' : copyState === 'failed' ? 'Copy failed' : 'Copy'}</span>
      </button>
    </div>
    {/* Long lines scroll inside this keyboard-accessible region instead of widening the chat. */}
    <div role="region" aria-label={`${declared || 'Plain text'} code`} tabIndex={0} className="max-h-[28rem] min-w-0 max-w-full overflow-auto overscroll-contain focus-visible:outline focus-visible:outline-2 focus-visible:outline-violet-700">
      {highlight ? <SyntaxHighlighter language={language} style={tokens} customStyle={{ margin: 0, padding: '14px', background: 'transparent', color: '#1e293b', fontSize: 12, lineHeight: '1.7', whiteSpace: 'pre', overflow: 'visible' }} codeTagProps={{ style: { fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace', whiteSpace: 'pre', overflowWrap: 'normal', wordBreak: 'normal' } }}>{code}</SyntaxHighlighter> : <pre className="m-0 whitespace-pre p-3.5 text-xs leading-[1.7] text-slate-800"><code>{code}</code></pre>}
    </div>
  </div>;
}

/** Inline fragments remain distinguishable without receiving block controls or breaking surrounding prose. */
function InlineCode({ children }: ComponentPropsWithoutRef<'code'>) {
  return <code className="rounded bg-slate-200/60 px-1 py-0.5 font-mono text-[0.92em] text-slate-800 [overflow-wrap:anywhere]">{children}</code>;
}

/** Avoids remote image fetches from model-authored Markdown while retaining useful alternative text. */
function HiddenRemoteImage({ alt }: { alt?: string }) { return <span>{alt || 'Image'}</span>; }

/** Shares safe Markdown rendering for assistant examples and user-pasted code in every chat layout. */
export function FusedAgentMarkdown({ text }: { text: string }) {
  return <div className="min-w-0 max-w-full break-words text-[13px] leading-[1.75] text-slate-600 [&_p]:mb-3 [&_p:last-child]:mb-0 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:list-decimal [&_ol]:pl-5 [&_a]:underline [&_table]:block [&_table]:max-w-full [&_table]:overflow-x-auto [&_th]:border [&_th]:p-2 [&_td]:border [&_td]:p-2 [&_h2]:font-semibold [&_h3]:font-semibold">
    <ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ pre: CodeBlock, code: InlineCode, img: HiddenRemoteImage }}>{text}</ReactMarkdown>
  </div>;
}
