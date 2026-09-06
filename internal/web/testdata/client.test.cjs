const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const client = fs.readFileSync(path.join(__dirname, '../assets/static/app.js'), 'utf8');

class Element {
  constructor() {
    this.dataset = {};
    this.children = [];
    this.listeners = {};
    this.textContent = '';
    this.hidden = true;
    this.disabled = false;
  }
  addEventListener(name, handler) { this.listeners[name] = handler; }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  closest() { return this; }
}

function openJob(fetchResult = async () => ({ok: true})) {
  const ids = ['live-job', 'otp-code', 'otp-panel', 'pairing-candidates', 'pairing-panel',
    'job-message', 'job-pill', 'approval-panel', 'job-events', 'job-status', 'job-started',
    'job-finished', 'job-confirmation', 'cancel-job'];
  const nodes = Object.fromEntries(ids.map(id => [id, new Element()]));
  nodes['live-job'].dataset = {jobId: '42', csrf: 'synthetic-csrf', lastEventId: '7'};
  nodes['job-status'].textContent = 'queued';
  nodes['cancel-job'].hidden = false;
  nodes['cancel-job'].dataset.decision = 'cancel-job';
  const listeners = {};
  const events = {};
  const alerts = [];
  const requests = [];
  let closed = false;
  let destination = null;
  class EventSource {
    constructor(url) { assert.equal(url, '/jobs/42/events?after=7'); }
    addEventListener(name, handler) { events[name] = handler; }
    close() { closed = true; }
  }
  const window = {
    EventSource,
    location: {replace: value => { destination = value; }},
    addEventListener: (name, handler) => { listeners[name] = handler; },
  };
  vm.runInNewContext(client, {
    window, EventSource, URLSearchParams,
    document: {getElementById: id => nodes[id], createElement: () => new Element()},
    fetch: async (...args) => { requests.push(args); return fetchResult(...args); },
    alert: message => alerts.push(message),
  });
  return {nodes, listeners, alerts, requests,
    emit: (name, data) => events[name]({data: JSON.stringify(data)}),
    click: button => nodes['live-job'].listeners.click({target: button}),
    get closed() { return closed; },
    get destination() { return destination; },
  };
}

function showSensitiveState(job) {
  job.emit('otp', {active: true, code: '123456'});
  job.emit('pairing', {active: true, candidates: [
    {id: 'message-1', code: '654321', masked_sender: '***1234', service: 'SMS'},
  ]});
  assert.equal(job.nodes['otp-code'].textContent, '123456');
  assert.equal(job.nodes['pairing-candidates'].children.length, 1);
}

function assertSensitiveStateCleared(job) {
  assert.equal(job.nodes['otp-code'].textContent, '');
  assert.equal(job.nodes['otp-panel'].hidden, true);
  assert.equal(job.nodes['pairing-panel'].hidden, true);
  assert.equal(job.nodes['pairing-candidates'].children.length, 0);
}

for (const signal of ['error', 'terminal', 'auth_expired', 'pagehide']) {
  test(`${signal} removes already displayed OTPs and pairing candidates`, () => {
    const job = openJob();
    showSensitiveState(job);
    if (signal === 'terminal') job.emit('state', {terminal: true, label: 'complete'});
    else if (signal === 'pagehide') job.listeners.pagehide();
    else job.emit(signal, {});
    assertSensitiveStateCleared(job);
    if (signal === 'terminal') assert.equal(job.closed, false, 'final events may still be in transit');
    if (signal === 'auth_expired') assert.equal(job.closed, true);
    if (signal === 'auth_expired') assert.equal(job.destination, '/login');
  });
}

test('live completion updates all details and controls before waiting for the final events', async () => {
  const job = openJob();
  const running = {status: 'running', label: 'running', class_name: 'active', message: 'Checking passes',
    started: 'Started now', finished: '—', confirmation_started: '—', can_cancel: true, awaiting_approval: false, terminal: false};
  job.emit('state', running);
  assert.equal(job.nodes['cancel-job'].hidden, false);
  assert.equal(job.nodes['job-status'].textContent, 'running');
  assert.equal(job.nodes['job-started'].textContent, 'Started now');
  showSensitiveState(job);

  job.emit('state', {...running, status: 'succeeded', label: 'succeeded', class_name: 'ok',
    message: 'Booking confirmed', finished: 'Finished now', confirmation_started: 'Confirmed now',
    terminal: true, can_cancel: false});
  assert.equal(job.nodes['job-pill'].textContent, 'succeeded');
  assert.equal(job.nodes['job-pill'].className, 'pill ok');
  assert.equal(job.nodes['job-status'].textContent, 'succeeded');
  assert.equal(job.nodes['job-message'].textContent, 'Booking confirmed');
  assert.equal(job.nodes['job-finished'].textContent, 'Finished now');
  assert.equal(job.nodes['job-confirmation'].textContent, 'Confirmed now');
  assert.equal(job.nodes['cancel-job'].hidden, true);
  assert.equal(job.nodes['cancel-job'].disabled, true);
  assert.equal(job.nodes['approval-panel'].hidden, true);
  assertSensitiveStateCleared(job);
  assert.equal(job.closed, false, 'terminal state must not drop the final durable events');

  job.emit('otp', {active: true, code: '123456'});
  job.emit('pairing', {active: true, candidates: [{id: 'late', code: '123456'}]});
  assertSensitiveStateCleared(job);
  await job.click(job.nodes['cancel-job']);
  assert.equal(job.requests.length, 0);

  job.emit('job_event', {id: 8, time: '12:00:00', type: 'job.succeeded', message: 'Booking confirmed'});
  assert.equal(job.nodes['job-events'].children.length, 1);
  assert.equal(job.nodes['job-events'].children[0].children[1].children[1].textContent, 'Booking confirmed');
  job.emit('complete', {});
  assert.equal(job.closed, true);
});

test('replayed events do not duplicate server-rendered or already received history', () => {
  const job = openJob();
  for (const id of [6, 7, 8, 8, 9]) {
    job.emit('job_event', {id, time: '12:00:00', type: 'progress', message: `Event ${id}`});
  }
  assert.equal(job.nodes['job-events'].children.length, 2);
});

test('pairing decision submits only the chosen message ID and CSRF token', async () => {
  const job = openJob();
  showSensitiveState(job);
  const button = job.nodes['pairing-candidates'].children[0];
  await job.click(button);
  const [url, request] = job.requests[0];
  assert.equal(url, '/jobs/42/decision');
  assert.equal(request.method, 'POST');
  assert.equal(request.credentials, 'same-origin');
  assert.deepEqual(Object.fromEntries(request.body), {
    csrf_token: 'synthetic-csrf', decision: 'pair', message_id: 'message-1',
  });
  assert.equal(button.disabled, true);
});

test('network failure leaves the decision usable and warns before a manual retry', async () => {
  const job = openJob(async () => { throw new Error('network unavailable'); });
  const button = new Element();
  button.dataset.decision = 'approve';
  await job.click(button);
  assert.equal(button.disabled, false);
  assert.equal(job.requests.length, 1, 'the browser must not retry decisions automatically');
  assert.match(job.alerts[0], /Check the job status before retrying/);
});

test('a rejected decision reports the server response and reenables the control', async () => {
  const job = openJob(async () => ({ok: false, text: async () => 'Approval expired'}));
  const button = new Element();
  button.dataset.decision = 'approve';
  await job.click(button);
  assert.equal(button.disabled, false);
  assert.deepEqual(job.alerts, ['Approval expired']);
});

test('a late rejected decision cannot reenable cancellation after the job finishes', async () => {
  let resolveRequest;
  const job = openJob(() => new Promise(resolve => { resolveRequest = resolve; }));
  const button = job.nodes['cancel-job'];
  const pending = job.click(button);
  job.emit('state', {status: 'succeeded', terminal: true, can_cancel: false});
  resolveRequest({ok: false, text: async () => 'Job already finished'});
  await pending;
  assert.equal(button.hidden, true);
  assert.equal(button.disabled, true);
});
