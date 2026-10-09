import { useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { ServiceDetailsEditor } from '../../app/components/integration-details/ServiceDetailsEditor';
import { ToastProvider } from '../../app/components/Toast';
import { api, type Service } from '../../app/lib/api';

let saved = {name:'Agent Import Demo',description:'A service for testing inbound events.'};
/** Keep preview saves entirely in memory; no real account or service is updated. */
api.integrations.updateDetails = async (_id, details) => { saved = details; };

/** Render the production owner editor with synthetic data and an observable save result. */
function Preview() {
 const anchor = useRef<HTMLButtonElement>(null);
 const [editing,setEditing] = useState(true);
 const [details,setDetails] = useState(saved);
 const [owner,setOwner] = useState(true);
 /** Closing a draft must not copy its fields into saved service details. */
 function close() { setEditing(false); }
 /** Simulate refreshing the server projection after a completed save. */
 function complete() { setDetails(saved); setEditing(false); }
 /** Reopen with the last saved data, as the real service actions menu does. */
 function edit() { setEditing(true); }
 /** Verify that non-owners cannot render the editor even if requested directly. */
 function toggleOwner() { setOwner(!owner); }
 return <ToastProvider><main className="min-h-dvh bg-slate-50 p-8 text-slate-900"><h1 className="text-2xl font-semibold">{details.name}</h1><p className="my-3">{details.description}</p><p className="mb-5 text-sm text-slate-500">Local preview · no live changes</p><button ref={anchor} className="rounded-lg border bg-white px-4 py-2" onClick={edit}>Edit details</button><button className="ml-3 rounded-lg border bg-white px-4 py-2" onClick={toggleOwner}>Toggle owner</button>{/* Only the production component decides whether the supplied owner may edit. */}{editing && <ServiceDetailsEditor anchor={anchor} service={{id:'preview',...details,is_owner:owner} as Service} onClose={close} onSaved={complete}/>}</main></ToastProvider>;
}
createRoot(document.getElementById('root')!).render(<Preview/>);
