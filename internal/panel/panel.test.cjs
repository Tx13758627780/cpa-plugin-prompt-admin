const {test}=require('node:test');
const assert=require('node:assert/strict');
const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const script=readFileSync(__dirname+'/index.html','utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
function fixture(path='/v0/resource/plugins/prompt-admin/index.html'){
 const elements=new Map();
 class Element{
  constructor(){this.nodeType=1;this.children=[];this.style={};this.value='';this.textContent='';this.checked=false;this.disabled=false;this.hidden=new Set(['hidden']);this.classList={add:x=>this.hidden.add(x),remove:x=>this.hidden.delete(x),contains:x=>this.hidden.has(x),toggle:(x,force)=>force?this.hidden.add(x):this.hidden.delete(x)}}
  append(...items){this.children.push(...items)}
  set innerHTML(value){this.children=[]}
  setAttribute(k,v){this[k]=v}
  addEventListener(k,v){this['on'+k]=v}
 }
 const storage=()=>{const values=new Map();return {getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)}};
 const window={location:{pathname:path,host:'proxy.example.com'},localStorage:storage(),sessionStorage:storage()};
 const document={querySelector:s=>{if(!elements.has(s))elements.set(s,new Element());return elements.get(s)},createElement:()=>new Element(),createTextNode:text=>({nodeType:3,textContent:text}),visibilityState:'visible'};
 const context=vm.createContext({window,document,navigator:{userAgent:'test'},TextEncoder,TextDecoder,atob,btoa,Uint8Array,AbortController,setTimeout,clearTimeout,setInterval:()=>1,clearInterval:()=>{},fetch:async()=>{throw new Error('unexpected fetch')},alert:()=>{},confirm:()=>true});
 vm.runInContext(script,context);
 window.localStorage.setItem('managementKey',JSON.stringify('management-secret'));
 const run=code=>vm.runInContext(code,context);
 return {context,run,get:s=>document.querySelector(s)};
}
const response=(status,body)=>({status,ok:status>=200&&status<300,statusText:'Upstream error',text:async()=>typeof body==='string'?body:JSON.stringify(body)});
const state=()=>({repos:[],sync:{running:false},status:{models:[],rows:[],model_error:'HTTP 401: Invalid API key'},overrides:{},local:{},manual_models:[]});

test('same-origin plugin requests retain proxy prefixes and management authentication',async()=>{
 for(const path of ['/v0/resource/plugins/prompt-admin/index.html','/cpa/v0/resource/plugins/prompt-admin/index.html','/cpa/v8/resource/plugins/prompt-admin/index.html']){
  const f=fixture(path);const prefix=path.startsWith('/cpa/')?'/cpa':'';
  f.context.fetch=async(url,opt)=>{assert.equal(url,prefix+'/v0/management/plugins/prompt-admin/state');assert.equal(opt.credentials,'same-origin');assert.equal(opt.headers.Authorization,'Bearer management-secret');return response(200,state())};
  await f.run('load()');
  assert.equal(f.get('#app').classList.contains('hidden'),false);
  assert.match(f.get('#statusErr').textContent,/HTTP 401/);
 }
});

test('HTTP errors and HTML/invalid JSON remain visible on initial load',async()=>{
 for(const [status,body,expected] of [[403,{error:'Forbidden'},/HTTP 403.*Forbidden/],[502,'<html>Bad gateway</html>',/HTTP 502.*不是 JSON/],[200,{wrong:true},/缺少 status/],[401,{error:'expired'},/HTTP 401.*expired/]]){
  const f=fixture();f.context.fetch=async()=>response(status,body);
  assert.equal(await f.run('load()'),false);
  assert.equal(f.get('#pageErr').classList.contains('hidden'),false);
  assert.match(f.get('#pageErrText').textContent,expected);
  if(status===401)assert.match(f.get('#authNotice').textContent,/HTTP 401/);
 }
});

test('network failures are visible and retries clear the error',async()=>{
 const f=fixture();f.context.fetch=async()=>{throw new Error('connection failed')};
 await f.run('load()');assert.match(f.get('#pageErrText').textContent,/网络请求失败.*connection failed/);
 f.context.fetch=async()=>response(200,state());await f.get('#retryBtn').onclick();
 assert.equal(f.get('#pageErr').classList.contains('hidden'),true);
});

test('refresh requests recomputation and manual models persist through the management API',async()=>{
 const f=fixture();const calls=[];
 f.context.fetch=async(url,opt)=>{calls.push([url,opt.body&&JSON.parse(opt.body)]);return response(200,state())};
 await f.get('#refreshModels').onclick();
 assert.equal(calls[0][0],'/v0/management/plugins/prompt-admin/models/refresh');
 assert.equal(f.get('#refreshModels').disabled,false);
 f.get('#manualModel').value=' custom-model ';
 await f.get('#addManualModel').onclick();
 assert.deepEqual(calls[1],['/v0/management/plugins/prompt-admin/models/manual',{model:'custom-model'}]);
 assert.equal(f.get('#manualModel').value,'');
 assert.match(f.get('#models').children[0].children[0].children[0].textContent,/未加载到模型/);
});

test('401, 404 and non-JSON action failures do not become empty objects',async()=>{
 for(const [status,body] of [[401,{error:'expired'}],[404,{error:{message:'missing route'}}],[200,'invalid']]){
  const f=fixture();f.context.fetch=async()=>response(status,body);
  await assert.rejects(f.run("api('state')"),new RegExp('HTTP '+status));
 }
});
