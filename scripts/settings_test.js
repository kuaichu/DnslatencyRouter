const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html = fs.readFileSync(process.argv[2] || path.join(__dirname,'../internal/web/dashboard.html'),'utf8');
const line = marker => {const found=html.split(/\r?\n/).find(l=>l.startsWith(marker));assert(found,marker);return found;};
async function checkSettings(profiles) {
  const ids=new Set([...html.matchAll(/\bid="([^"]+)"/g)].map(m=>m[1]));
  const elements=new Map();
  const element=id=>{if(!ids.has(id))return null;if(!elements.has(id)){const classes=new Set();elements.set(id,{value:'',textContent:'',classList:{add:v=>classes.add(v),contains:v=>classes.has(v)},focus(){}});}return elements.get(id);};
  const toasts=[];
  const cfg={target_domain:'entry.example',alert_webhook_url:'https://example.com/test-webhook',cloudflare_api_token_set:true,agent_token_set:true,agents:[],ping_mode:'tcp',ping_port:443};
  const context=vm.createContext({$:element,fetch:async()=>({json:async()=>cfg}),loadAirportProfiles:async()=>({airport_profiles:profiles}),mergeAgentPeers:peers=>peers,renderAgentPeersEditor(){},updateAgentInstallCommand(){},updatePortFieldState(){},configureSettingsMode(){},lockBodyScroll(){},randomToken:()=> 'test-token',showToast:(text)=>toasts.push(text),window:{location:{origin:'http://localhost'}},setTimeout:callback=>callback(),lastStatus:null,activeSettingsTab:'airports',agentTokenConfigured:false,agentPeersDraft:[]});
  vm.runInContext(line('const els='),context);
  vm.runInContext(line('function openSettings('),context);
  context.openSettings();
  await new Promise(resolve=>setImmediate(resolve));
  assert.deepEqual(toasts,[], 'opening settings unexpectedly showed an error');
  assert(element('settingsModal').classList.contains('active'),'settings dialog did not open');
  assert.equal(element('inputAlertWebhookURL').value,cfg.alert_webhook_url);
  // Catch missing bindings used by any settings save/read handler too.
  const names=new Set([...html.matchAll(/\bels\.([A-Za-z_$][\w$]*)/g)].map(m=>m[1]));
  for(const name of names)assert(vm.runInContext(`Object.prototype.hasOwnProperty.call(els, ${JSON.stringify(name)})`,context),`Missing els binding: ${name}`);
}
(async()=>{await checkSettings([]);await checkSettings([{id:'airport-a'}]);console.log('Settings open tests passed (legacy and multi-airport)');})().catch(error=>{console.error(error);process.exitCode=1;});
