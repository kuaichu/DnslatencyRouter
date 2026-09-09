const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, '../internal/web/dashboard.html'), 'utf8');
for (const match of html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi)) new vm.Script(match[1]);
function source(name) {
  const start = html.indexOf('function ' + name + '(');
  assert(start >= 0);
  return html.slice(start, html.indexOf('\n}', start) + 2);
}
const context = vm.createContext({agentStatusText: state => ({online:'在线',offline:'离线',stale:'过期'}[state] || '未知')});
vm.runInContext(source('taskDetailModel'), context);
const agent = {id:'agent-a',status:'online'};
const profile = {id:'airport-a'};
const report = {profiles:[{
  profileId:'airport-a',startedAt:'2026-09-09T10:00:00Z',finishedAt:'2026-09-09T10:00:05Z',
  resolvedIps:['1.1.1.1','8.8.8.8','1.1.1.1'],
  results:[{ip:'1.1.1.1',latency:10,successes:2},{ip:'1.1.1.1',latency:11,successes:1},{ip:'8.8.8.8',latency:20,error:'timeout'},{ip:'9.9.9.9',latency:0},{ip:'4.4.4.4',latency:2,successes:0}]
}]};
let model = context.taskDetailModel(agent, report, profile);
assert.equal(model.resolved,2);
assert.equal(model.results,5);
assert.equal(model.successful,1);
assert.equal(model.elapsed,'5.0 秒');
for (const args of [[agent,null,profile],[agent,report,{id:'other'}],[{...agent,status:'stale'},report,profile],[null,report,profile]]) {
  const empty=context.taskDetailModel(...args);
  assert.equal(empty.successful,null);
  assert.equal(empty.results,null);
}
const failed = context.taskDetailModel(agent,{profiles:[{...report.profiles[0],error:'DNS failed'}]},profile);
assert.equal(failed.successful,null);
assert.match(failed.notice,/DNS failed/);
const zero=context.taskDetailModel(agent,{profiles:[{profileId:'airport-a',resolvedIps:[],results:[]}]},profile);
assert.equal(zero.successful,0);
assert.equal(zero.resolved,0);
const elements=Object.fromEntries(['attemptsSide','improvementSide','stableSide'].map(key=>[key,{textContent:''}]));
context.document={getElementById:id=>elements[id]};
vm.runInContext(source('renderRuntimeSummary'),context);
context.renderRuntimeSummary({pingAttempts:4,switchImprovement:0,switchStableSec:0});
assert.equal(elements.attemptsSide.textContent,'4 次');
assert.equal(elements.improvementSide.textContent,'0%');
assert.equal(elements.stableSide.textContent,'0 秒');
context.renderRuntimeSummary({});
assert.equal(elements.attemptsSide.textContent,'-');
console.log('Agent task details and runtime summary tests passed');
