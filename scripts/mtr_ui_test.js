const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, '../internal/web/dashboard.html'), 'utf8');
for (const match of html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi)) new vm.Script(match[1]);
const start=html.indexOf('function mtrTimingValues(');
const end=html.indexOf('async function refreshMTRView(',start);
const ctx=vm.createContext({escapeHTML:value=>String(value??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;'),Date});
ctx.lastStats=[];
for(const name of ['formatGeoLabel','countryKeyFromGeo','countryLabelFromKey','geoFlagHTML'])vm.runInContext(html.split(/\r?\n/).find(line=>line.startsWith('function '+name+'(')),ctx);
vm.runInContext(html.slice(start,end),ctx);
const result=ctx.mtrCardHTML({id:'job',ip:'1.1.1.1',port:12001,status:'completed',output:'HOST: source\n 1. <script>alert(1)</script>',finishedAt:'2026-09-14T00:00:00Z'});
assert.match(result,/结果待确认/);assert.match(result,/12001/);assert.match(result,/&lt;script&gt;/);assert(!result.includes('<script>'));
assert.match(ctx.mtrCardHTML({id:'x',ip:'8.8.8.8',port:443,status:'failed',error:'missing binary'}),/追踪失败/);
const output=`NextTrace v1.7.3
[NextTrace API] preferred API IP - 103.117.102.27 - 999.00ms - DMIT.NRT
IP Geo Data Provider: NextTrace-API
10.0.0.240 -> 34.92.149.218, 30 hops max, 44 byte packets, TCP mode
1   10.0.0.1        *                         RFC1918
                                              0.50 ms / 0.70 ms / * ms
2   101.64.40.1     AS4837   [UNICOM-ZJ]      中国 浙江省 宁波市  chinaunicom.cn  联通
                                              5.00 ms / 7.00 ms
3   221.12.35.209   AS4837   [UNICOM-ZJ]      中国 浙江省 宁波市  chinaunicom.cn  联通
                                              9.00 ms
4   *
5   *
6   203.131.244.20  AS2914   [NTT-GLOBAL]     中国 香港   gin.ntt.net
    ce-2-3.a01.hk.bb.gin.ntt.net              110.00 ms / 130.00 ms / * ms
    [MPLS: Lbl 242693, TC 0, S 1, TTL 1]
    129.250.6.65   AS2914   [NTT-BACKBONE]   新加坡 gin.ntt.net
    ae-15.sngpsi07.sg.bb.gin.ntt.net          88.00 ms / 90.00 ms
7   34.92.149.218   AS396982                  中国 香港 google.com
    218.149.92.34.bc.googleusercontent.com    68.00 ms / 72.00 ms / * ms
Trace Stopped: Destination Reached at Hop 7 (TCP RST from 34.92.149.218)
MapTrace URL: https://example.com/123`;
const row={id:'route-a',ip:'34.92.149.218',port:12001,status:'completed',output,finishedAt:'2026-09-14T00:00:00Z'};
const route=ctx.parseMTRRoute(row);
assert.equal(route.hops.length,7);
assert.equal(route.hops[5].responders.length,2,'multi-IP TTL must keep both responses');
assert.equal(route.hops[6].responders[0].hostname,'218.149.92.34.bc.googleusercontent.com');
assert.equal(ctx.mtrAverage(route.targetValues),70,'use only target RTT, not Geo API or transit RTT');
assert.equal(route.reached,true);
assert.equal(route.hops[1].responders[0].location,'中国 浙江省 宁波市');
assert.equal(route.hops[1].responders[0].network,'chinaunicom.cn 联通');
assert.equal(ctx.mtrAverage([null]),null,'timeouts are not zero latency');
assert.equal(ctx.mtrAverage([0]),0,'valid zero latency is retained');
const card=ctx.mtrCardHTML(row);
assert.match(card,/已到达目标/);
assert.match(card,/70\.0/);
assert.match(card,/2\/3/);
assert.match(card,/第 4–5 跳未回应/);
assert.match(card,/data-mtr-detail="route-a\|hops"/);
assert.match(card,/<details class="mtr-raw"[^>]*><summary>技术详情/);
assert(!card.includes(' open'),'raw output stays collapsed');
assert.equal((ctx.mtrPathHTML(route).match(/中国 浙江省 宁波市/g)||[]).length,1,'adjacent repeated locations merge');
const maxHops={...row,ip:'8.8.8.8',output:output.replace(/34\.92\.149\.218, 30 hops max/,'8.8.8.8, 30 hops max').replace(/Destination Reached at Hop 7.*\n/,'Maximum Hops Reached at Hop 7 (No Destination Response)\n')};
assert.equal(ctx.mtrAverage(ctx.parseMTRRoute(maxHops).targetValues),null,'last transit node is never used as target');
assert.match(ctx.mtrCardHTML(maxHops),/已到达跳数上限/);
assert(!ctx.mtrCardHTML(maxHops).includes('已到达目标'));
assert.match(ctx.mtrCardHTML({...row,output:output.replace(/Destination Reached at Hop 7.*/, 'No Continuing Route Observed at Hop 7 (ICMP Host Unreachable (!H))')}),/收到不可达回应/);
assert.match(ctx.mtrCardHTML({...row,status:'failed',error:'timeout'}),/本次追踪未能完成/);
assert.match(ctx.mtrCardHTML({...row,status:'pending',output:''}),/等待追踪/);
assert.match(ctx.mtrCardHTML({...row,output:output.slice(0,output.indexOf('Trace Stopped:'))}),/结果待确认/);
const legacy=ctx.parseMTRRoute({...row,output:'HOST: source Loss% Snt\n 1. 1.1.1.1 0% 10'});
assert.equal(legacy.hops.length,0);
const hostile={...row,id:'"><img src=x>',output:output.replace('中国 香港 google.com','<img src=x onerror=alert(1)>')};
assert(!ctx.mtrCardHTML(hostile).includes('<img'));
assert.equal(ctx.parseMTRRoute({...row,output:output.replace('AS4837','\x1b[32mAS4837\x1b[0m')}).hops[1].responders[0].asn,'AS4837');
console.log('Route parsing, summary accuracy, states, escaping and legacy fallback passed');
ctx.lastStats=[{ip:row.ip,geo:'Hong Kong - Google Cloud'}];
assert.match(ctx.mtrCardHTML(row),/<span class="region-geo mtr-geo"><img[^>]+src="\/assets\/flags\/hk.svg"[^>]*>Hong Kong · Google Cloud<\/span>/);
ctx.lastStats=[{ip:'8.8.8.8',geo:'United States - Google'}];
assert(!ctx.mtrCardHTML(row).includes('United States'),'geo must belong to target IP');
ctx.lastStats=[{ip:row.ip,geo:'<img src=x> - Example IDC'}];
assert(!ctx.mtrCardHTML(row).includes('<img src=x>'),'geo must be escaped');
for(const [geo,key] of [['Japan Osaka - Amazon.com','jp'],['Singapore - Amazon.com','sg'],['Malaysia Kuala Lumpur - Amazon.com','my'],['Unrecognized location - IDC','xx']]){
  ctx.lastStats=[{ip:row.ip,geo}];
  assert(ctx.mtrCardHTML(row).includes('/assets/flags/'+key+'.svg'));
  assert(fs.existsSync(path.join(__dirname,'../internal/web/assets/flags',key+'.svg')));
}
console.log('Target IP region and IDC labels passed');
