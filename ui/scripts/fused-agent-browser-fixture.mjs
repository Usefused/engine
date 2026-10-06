import { createServer } from 'vite';
import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
const ui=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const state={session:0,turn:0,pages:[],outputs:[],field:null,intent:""};
/** Returns synthetic Harnest protocol data without reaching live services. */
function json(res,data){res.setHeader('Content-Type','application/json');res.end(JSON.stringify(data));}
/** Bounds fixture input while preserving actual tool output for privacy checks. */
async function body(req){let text='';for await(const chunk of req){text+=chunk;if(text.length>1000000)throw Error('fixture body too large');}return JSON.parse(text||'{}');}
/** Exercises frontend pause/resume through actual production tools. */
async function handle(req,res,next){
 const pathname=req.url.split('?')[0];
 // Fixture endpoints are isolated from any real account or provider.
 if(pathname==='/agent/status')return json(res,{status:'ready',enabled:true});
 if(pathname==='/fixture/results')return json(res,{sessions:state.session,turns:state.turn,privateValueLeaked:JSON.stringify(state.outputs).includes('PRIVATE-FIXTURE-VALUE'),pages:state.pages});
 if(pathname==='/agent/sessions'){state.session++;return json(res,{id:`session-${state.session}`});}
 if(pathname==='/agent/responses'){
  state.intent=(await body(req)).input;state.turn++;
  const call={id:`page-${state.turn}`,callId:'read',name:'get_page_context',arguments:{}};
  res.setHeader('Content-Type','text/event-stream');res.end(`data: ${JSON.stringify({type:'response.completed',status:'requires_action',requiredAction:{type:'client_tool',...call}})}\n\n`);return;
 }
 if(pathname.startsWith('/agent/client-tools/')){
  const input=await body(req);state.outputs.push(input);
  // Context must identify a real field before requesting an edit.
  if(pathname.includes('/page-')){
   if(input.output.ok===false)return json(res,{status:'completed',outputText:'Page context is disabled. Enable it to inspect or edit this form.'});
   const page=input.output;state.pages.push({revision:page.revision,fields:page.fields});state.field=page.fields.find((field)=>field.label==='App name');
   if(/change|rename/i.test(state.intent))return json(res,{status:'requires_action',requiredAction:{type:'client_tool',id:`edit-${state.turn}`,callId:'edit',name:'update_form_field',arguments:{expected_revision:page.revision,field_id:state.field.id,value:'Customer checkout'}}});
   return json(res,{status:'completed',outputText:`The app name is ${state.field.value}. The signing secret is private and was not included in my context.`});
  }
  return json(res,{status:'completed',outputText:input.output.updated?'Changed the app name to Customer checkout. Your change is in the form and has not been saved.':'The form rejected the edit.'});
 }
 return next();
}
const server=await createServer({configFile:false,root:path.join(ui,'testdata/fused-agent'),resolve:{alias:{'~':path.join(ui,'app')}},esbuild:{jsx:'automatic'},server:{host:'127.0.0.1',port:18196,strictPort:true},plugins:[{name:'fused-agent-fixture',configureServer(vite){
 // Use production CSS so screenshots exercise the real responsive design.
 vite.middlewares.use((req,res,next)=>{if(req.url!='/fixture.css')return next();const assets=path.join(ui,'build/client/assets');res.setHeader('Content-Type','text/css');res.end(readFileSync(path.join(assets,readdirSync(assets).find((file)=>file.endsWith('.css')))));});
 vite.middlewares.use((req,res,next)=>{handle(req,res,next).catch(()=>{res.statusCode=500;json(res,{error:'fixture failed'});});});
}}]});
await server.listen();console.log('Fused agent fixture: http://127.0.0.1:18196');
