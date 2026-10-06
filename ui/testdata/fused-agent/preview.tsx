import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter } from 'react-router-dom';
import { FusedAgentProvider } from '../../app/components/agent/FusedAgentProvider';
import { TypeScriptEditor } from '../../app/components/code/TypeScriptEditor';

/** Exercises production context and editing with synthetic credentials and ordinary controlled React fields. */
function Preview() {
 const [name,setName]=useState('Customer brief');
 const [source,setSource]=useState('export default buildUnifiedApp({\n  async execute({ input }) {\n    return { customer: input.customer };\n  }\n});');
 return <MemoryRouter><FusedAgentProvider authenticated><main data-fused-workspace className="min-h-screen p-6 sm:p-10"><header className="mb-10 flex items-center justify-between"><strong className="text-xl">FUSED</strong><span className="text-xs text-slate-500">Local UI verification · sample data</span></header><div className="max-w-3xl space-y-6"><div><p className="text-sm text-violet-600">Unified Apps</p><h1 className="mt-2 text-3xl font-bold text-slate-900">Review your app</h1></div><section className="space-y-5 rounded-xl border border-slate-200 bg-white p-6"><label className="block text-sm font-medium text-slate-900">App name<input value={name} onChange={(event)=>setName(event.target.value)} className="mt-2 block w-full rounded-lg border border-slate-300 p-3 font-normal" /></label><label className="block text-sm font-medium text-slate-900">Signing secret<input type="text" data-fused-visible="false" defaultValue="PRIVATE-FIXTURE-VALUE" className="mt-2 block w-full rounded-lg border border-slate-300 p-3 font-normal" /></label><p className="text-xs text-slate-500">The visible sample secret is excluded from agent context.</p><TypeScriptEditor id="fixture-source" value={source} onChange={setSource} /><button type="button" className="rounded-lg bg-slate-950 px-4 py-2 text-sm text-white">Validate and compile</button></section></div></main></FusedAgentProvider></MemoryRouter>;
}
createRoot(document.getElementById('root')!).render(<Preview />);
