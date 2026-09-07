const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const client = fs.readFileSync(path.join(__dirname, '../assets/static/app.js'), 'utf8');

class Element {
  constructor(tagName = 'div') {
    this.tagName = tagName.toUpperCase();
    this.dataset = {};
    this.attributes = {};
    this.children = [];
    this.listeners = {};
    this._text = '';
    this.hidden = true;
    this.disabled = false;
  }
  get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
  set textContent(value) { this.replaceChildren(); this._text = value; }
  setAttribute(name, value) { this.attributes[name] = value; }
  getAttribute(name) { return this.attributes[name]; }
  addEventListener(name, handler) { this.listeners[name] = handler; }
  append(...children) {
    for (const child of children) { child.parentNode = this; this.children.push(child); }
  }
  replaceChildren(...children) {
    for (const child of this.children) child.parentNode = null;
    this.children = []; this._text = ''; this.append(...children);
  }
  remove() {
    this.parentNode.children = this.parentNode.children.filter(child => child !== this);
    this.parentNode = null;
  }
  matches(selector) {
    if (selector.startsWith('.')) return (this.className || '').split(' ').includes(selector.slice(1));
    const attribute = selector.match(/^\[data-([\w-]+)\]$/);
    return Boolean(attribute && Object.hasOwn(this.dataset, attribute[1].replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())));
  }
  querySelectorAll(selector) {
    return this.children.flatMap(child => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) { return this.matches(selector) ? this : this.parentNode?.closest(selector); }
}

function openPage({fetchResult = async () => new Response(null, {status: 204}), liveJob = true, supportsEvents = true, flash} = {}) {
  const ids = ['live-job', 'otp-code', 'otp-panel', 'pairing-candidates', 'pairing-panel',
    'job-message', 'job-pill', 'approval-panel', 'job-events', 'job-status', 'job-started',
    'job-finished', 'job-confirmation', 'cancel-job', 'notifications'];
  const nodes = Object.fromEntries(ids.map(id => [id, new Element()]));
  if (flash) nodes.notifications.append(flash);
  nodes['live-job'].dataset = {jobId: '42', csrf: 'synthetic-csrf', lastEventId: '7'};
  nodes['job-status'].textContent = 'queued';
  nodes['cancel-job'].hidden = false;
  nodes['cancel-job'].dataset.decision = 'cancel-job';
  const listeners = {};
  const events = {};
  const requests = [];
  let closed = false;
  let destination = null;
  let connections = 0;
  class EventSource {
    constructor(url) { connections++; assert.equal(url, '/jobs/42/events?after=7'); }
    addEventListener(name, handler) { events[name] = handler; }
    close() { closed = true; }
  }
  const window = {
    EventSource: supportsEvents ? EventSource : undefined,
    location: {replace: value => { destination = value; }},
    addEventListener: (name, handler) => { listeners[name] = handler; },
  };
  vm.runInNewContext(client, {
    window, EventSource, URLSearchParams,
    document: {getElementById: id => id === 'live-job' && !liveJob ? null : nodes[id], createElement: tagName => new Element(tagName)},
    fetch: async (...args) => { requests.push(args); return fetchResult(...args); },
  });
  return {nodes, listeners, requests,
    emit: (name, data) => events[name]({data: JSON.stringify(data)}),
    click: button => nodes['live-job'].listeners.click({target: button}),
    get closed() { return closed; },
    get destination() { return destination; },
    get connections() { return connections; },
  };
}

function openJob(fetchResult) { return openPage({fetchResult}); }

function notificationMessages(page) {
  return page.nodes.notifications.querySelectorAll('.notification-message').map(message => message.textContent);
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

test('queued and cancellation display labels remain distinct from raw wire statuses', () => {
  const job = openJob();
  const queued = {status: 'queued', label: 'Waiting to start', class_name: 'active',
    message: 'Earliest start: Mon, Sep 7, 2026 at 6:30 AM UTC-07:00. The job may start later.',
    started: '—', finished: '—', confirmation_started: '—', can_cancel: true, awaiting_approval: false, terminal: false};
  job.emit('state', queued);
  assert.equal(job.nodes['job-status'].textContent, 'Waiting to start');
  assert.equal(job.nodes['job-pill'].textContent, 'Waiting to start');
  assert.equal(job.nodes['job-message'].textContent, queued.message);
  job.emit('state', {...queued, label: 'Cancellation requested', message: 'Cancellation requested.'});
  assert.equal(job.nodes['job-status'].textContent, 'Cancellation requested');
  assert.equal(job.nodes['job-message'].textContent, 'Cancellation requested.');
  assert.equal(job.closed, false);
});

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
  assert.match(notificationMessages(job)[0], /Check the job status before retrying/);
});

test('a rejected decision reports the server response and reenables the control', async () => {
  const job = openJob(async () => new Response('Approval expired', {status: 409}));
  const button = new Element();
  button.dataset.decision = 'approve';
  await job.click(button);
  assert.equal(button.disabled, false);
  assert.deepEqual(notificationMessages(job), ['Approval expired']);
});

for (const scenario of [
  {name: 'expired-session redirect to login', status: 200, redirected: true, url: 'https://example.test/login', contentType: 'text/html'},
  {name: 'required-password-change redirect to Account', status: 200, redirected: true, url: 'https://example.test/account', contentType: 'text/html'},
  {name: 'unexpected successful HTML page', status: 200, contentType: 'text/html; charset=utf-8'},
  {name: 'HTML error page', status: 403, contentType: 'text/html'},
  {name: 'redirect ending with no content', status: 204, redirected: true},
]) {
  test(`${scenario.name} prompts session review without exposing the page or accepting the decision`, async () => {
    const response = new Response(scenario.status === 204 ? null : '<html><body>Private account page</body></html>', {
      status: scenario.status, headers: scenario.contentType ? {'Content-Type': scenario.contentType} : {},
    });
    if (scenario.redirected) Object.defineProperty(response, 'redirected', {value: true});
    if (scenario.url) Object.defineProperty(response, 'url', {value: scenario.url});
    const job = openJob(async () => response);
    showSensitiveState(job);
    const button = new Element('button');
    button.dataset.decision = 'approve';
    await job.click(button);
    assert.equal(button.disabled, false);
    assert.equal(job.requests.length, 1, 'session recovery must not retry a decision');
    assertSensitiveStateCleared(job);
    const messages = notificationMessages(job);
    assert.equal(messages.length, 1);
    assert.match(messages[0], /Sign in or check Account/);
    assert.match(messages[0], /check the job status before retrying/);
    assert.doesNotMatch(messages[0], /Private account page|<html>/);
  });
}

test('an unexpected successful status does not leave the decision accepted', async () => {
  const job = openJob(async () => new Response('Unexpected success body', {status: 200}));
  const button = new Element('button');
  button.dataset.decision = 'approve';
  await job.click(button);
  assert.equal(button.disabled, false);
  assert.equal(job.requests.length, 1);
  assert.match(notificationMessages(job)[0], /Check the job status before retrying/);
  assert.doesNotMatch(notificationMessages(job)[0], /Unexpected success body/);
});

test('decision errors remain separate, safe text notifications until dismissed', async () => {
  const message = '<img src=x onerror="alert(1)">';
  const job = openJob(async () => new Response(message, {status: 409}));
  const button = new Element('button');
  button.dataset.decision = 'approve';
  await job.click(button);
  await job.click(button);
  assert.deepEqual(notificationMessages(job), [message, message]);
  const notices = job.nodes.notifications.querySelectorAll('[data-notification]');
  for (const notice of notices) {
    assert.equal(notice.getAttribute('role'), 'alert');
    assert.equal(notice.getAttribute('aria-atomic'), 'true');
    assert.equal(notice.querySelector('.notification-message').children.length, 0, 'server text must not become HTML');
  }
  const dismiss = notices[0].querySelector('[data-dismiss-notification]');
  assert.equal(dismiss.hidden, false);
  assert.equal(dismiss.type, 'button');
  assert.equal(dismiss.getAttribute('aria-label'), 'Dismiss notification');
  dismiss.listeners.click();
  assert.deepEqual(notificationMessages(job), [message], 'dismiss affects only its own notification');
  assert.equal(job.requests.length, 2, 'dismissing a notification must not submit a decision');
});

for (const options of [{liveJob: false}, {supportsEvents: false}]) {
  test(`server notifications can be dismissed without ${options.liveJob === false ? 'a live job' : 'EventSource support'}`, () => {
    const notice = new Element();
    notice.dataset.notification = '';
    const message = new Element('span');
    message.className = 'notification-message';
    message.textContent = 'This booking already has an active job.';
    const link = new Element('a');
    link.setAttribute('href', '/jobs/42');
    link.textContent = 'View job';
    const dismiss = new Element('button');
    dismiss.dataset.dismissNotification = '';
    notice.append(message, link, dismiss);
    assert.equal(dismiss.hidden, true, 'server markup hides the nonfunctional control until JS initializes');
    const page = openPage({...options, flash: notice});
    assert.equal(page.connections, 0);
    assert.equal(dismiss.hidden, false);
    assert.deepEqual(notificationMessages(page), [message.textContent]);
    assert.equal(link.getAttribute('href'), '/jobs/42', 'initialization must preserve the normal navigation action');
    dismiss.listeners.click();
    assert.equal(page.nodes.notifications.children.length, 0);
    assert.equal(page.requests.length, 0);
  });
}

for (const [name, response] of [
  ['conflict', new Response('Job already finished', {status: 409})],
  ['HTML session page', new Response('<html>Sign in</html>', {headers: {'Content-Type': 'text/html'}})],
]) {
  test(`a late ${name} cannot reenable cancellation after the job finishes`, async () => {
    let resolveRequest;
    const job = openJob(() => new Promise(resolve => { resolveRequest = resolve; }));
    const button = job.nodes['cancel-job'];
    const pending = job.click(button);
    job.emit('state', {status: 'succeeded', terminal: true, can_cancel: false});
    resolveRequest(response);
    await pending;
    assert.equal(button.hidden, true);
    assert.equal(button.disabled, true);
  });
}
