const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const root = require('node:path').resolve(__dirname, '..');

function sourceLine(file, marker) {
  const line = fs.readFileSync(require('node:path').join(root, file), 'utf8')
    .split(/\r?\n/)
    .find(item => item.includes(marker));
  assert(line, `missing ${marker} in ${file}`);
  return line;
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function response(value) {
  return { json: async () => value };
}

async function testDashboardSampleGeneration() {
  const oldResponse = deferred();
  const newResponse = deferred();
  const context = vm.createContext({
    selectedIP: '1.1.1.1', selectedAgentID: 'remote-a', selectedProfileID: 'profile-a',
    lastStatus: { profiles: [{ id: 'profile-a' }] }, lastSamples: [], ipSamples: [], sampleRequestSeq: 0,
    isControllerView: () => false,
    currentViewAgentID: () => context.selectedAgentID,
    getCurrentProfile: status => status.profiles[0],
    selectedSamplesURL: profile => `/api/ip-samples?profile_id=${profile.id}&ip=${context.selectedIP}&agent_id=${context.selectedAgentID}`,
    filterForCurrentView: (samples) => samples,
    drawChart: () => {},
    fetch: url => url.includes('1.1.1.1') ? oldResponse.promise : newResponse.promise,
    Promise,
    console
  });
  vm.runInContext(sourceLine('internal/web/dashboard.html', 'function selectedSamplesKey('), context);
  vm.runInContext(sourceLine('internal/web/dashboard.html', 'async function loadSelectedSamples('), context);

  const oldLoad = context.loadSelectedSamples({ id: 'profile-a' });
  context.selectedIP = '2.2.2.2';
  context.selectedAgentID = 'remote-b';
  context.selectedProfileID = 'profile-b';
  context.lastStatus = { profiles: [{ id: 'profile-b' }] };
  const newLoad = context.loadSelectedSamples({ id: 'profile-b' });
  newResponse.resolve(response([{ ip: '2.2.2.2' }]));
  await newLoad;
  oldResponse.resolve(response([{ ip: '1.1.1.1' }]));
  await oldLoad;
  assert.equal(JSON.stringify(context.lastSamples), JSON.stringify([{ ip: '2.2.2.2' }]));
  assert.equal(JSON.stringify(context.ipSamples), JSON.stringify([{ ip: '2.2.2.2' }]));

  const staleResponse = deferred();
  const currentResponse = deferred();
  let requestNumber = 0;
  context.fetch = () => (++requestNumber === 1 ? staleResponse.promise : currentResponse.promise);
  context.selectedIP = '3.3.3.3';
  const staleLoad = context.loadSelectedSamples({ id: 'profile-b' });
  context.selectedIP = '4.4.4.4';
  const currentLoad = context.loadSelectedSamples({ id: 'profile-b' });
  currentResponse.resolve(response([{ ip: '4.4.4.4' }]));
  await currentLoad;
  staleResponse.reject(new Error('old request failed'));
  await staleLoad;
  assert.equal(JSON.stringify(context.lastSamples), JSON.stringify([{ ip: '4.4.4.4' }]));

  const keyOnlyResponse = deferred();
  context.fetch = () => keyOnlyResponse.promise;
  context.selectedIP = '5.5.5.5';
  const keyOnlyLoad = context.loadSelectedSamples({ id: 'profile-b' });
  context.selectedIP = '6.6.6.6';
  keyOnlyResponse.resolve(response([{ ip: '5.5.5.5' }]));
  await keyOnlyLoad;
  assert.equal(JSON.stringify(context.lastSamples), JSON.stringify([{ ip: '4.4.4.4' }]));

  const emptyInvalidation = deferred();
  context.fetch = () => emptyInvalidation.promise;
  context.selectedIP = '7.7.7.7';
  const emptyLoad = context.loadSelectedSamples({ id: 'profile-b' });
  context.selectedIP = '';
  await context.loadSelectedSamples({ id: 'profile-b' });
  emptyInvalidation.resolve(response([{ ip: '7.7.7.7' }]));
  await emptyLoad;
  assert.equal(JSON.stringify(context.lastSamples), '[]');
  assert.equal(JSON.stringify(context.ipSamples), '[]');
}

async function testSingleFlight(file, marker, setup) {
  const gate = deferred();
  let calls = 0;
  const context = vm.createContext({
    ...setup,
    pollInFlight: false,
    fetch: () => { calls += 1; return gate.promise; },
    Promise,
    console
  });
  vm.runInContext(sourceLine(file, marker), context);
  const first = context.pollOnce();
  const second = context.pollOnce();
  assert.equal(calls, 5, `${file} poll must be single-flight`);
  gate.resolve(response([]));
  await Promise.all([first, second]);
  assert.equal(context.pollInFlight, false, `${file} poll must release its flight guard`);
  const next = context.pollOnce();
  assert.equal(calls, 10, `${file} poll must run again after completion`);
  gate.resolve(response([]));
  await next;
}

(async () => {
  await testDashboardSampleGeneration();
  await testSingleFlight('internal/web/dashboard.html', 'async function pollOnce(', {
    selectedIP: '', setStatus: () => {}, updateStatus: () => {}, refreshProfileView: () => {},
    loadSelectedSamples: async () => {}, renderLogs: () => {}, getCurrentProfile: () => null
  });
  console.log('ui poll tests passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
