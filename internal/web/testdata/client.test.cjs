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
  get selectedOptions() { return this.children.filter(child => child.tagName === 'OPTION' && child.value === this.value); }
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

function openPage({fetchResult = async () => new Response(null, {status: 204}), liveJob = true, supportsEvents = true, flash, booking, sourceForm} = {}) {
  const ids = ['live-job', 'otp-code', 'otp-panel', 'pairing-candidates', 'pairing-panel',
    'job-message', 'job-pill', 'approval-panel', 'job-events', 'job-status', 'job-started',
    'job-finished', 'job-confirmation', 'cancel-job', 'notifications'];
  const nodes = Object.fromEntries(ids.map(id => [id, new Element()]));
  if (booking) {
    nodes['booking-form'] = booking.form;
    nodes['lake-release-policy'] = booking.policy;
  }
  if (sourceForm) nodes['source-form'] = sourceForm;
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

test('source provider selection excludes inactive fields and preserves values when switching back', () => {
  const form = new Element('form');
  const selector = new Element('select');
  selector.value = 'twilio';
  form.elements = {namedItem: name => name === 'provider' ? selector : null};
  const bluebubbles = new Element('fieldset');
  bluebubbles.dataset.sourceProvider = 'bluebubbles';
  const serverURL = new Element('input');
  serverURL.value = 'unfinished URL';
  bluebubbles.append(serverURL);
  const twilio = new Element('fieldset');
  twilio.dataset.sourceProvider = 'twilio';
  const token = new Element('input');
  token.value = 'synthetic-unsaved-token';
  twilio.append(token);
  form.append(selector, bluebubbles, twilio);
  const page = openPage({liveJob: false, supportsEvents: false, sourceForm: form});
  assert.equal(bluebubbles.hidden, true);
  assert.equal(bluebubbles.disabled, true, 'inactive fieldset must not block validation or submit values');
  assert.equal(twilio.hidden, false);
  assert.equal(twilio.disabled, false);
  selector.value = 'bluebubbles';
  selector.listeners.change();
  assert.equal(bluebubbles.hidden, false);
  assert.equal(bluebubbles.disabled, false);
  assert.equal(serverURL.value, 'unfinished URL');
  assert.equal(twilio.hidden, true);
  assert.equal(twilio.disabled, true);
  selector.value = 'twilio';
  selector.listeners.change();
  assert.equal(twilio.hidden, false);
  assert.equal(twilio.disabled, false);
  assert.equal(token.value, 'synthetic-unsaved-token');
  assert.equal(bluebubbles.hidden, true);
  assert.equal(bluebubbles.disabled, true);
  assert.equal(page.requests.length, 0);
  assert.equal(page.connections, 0);
});

function bookingFixture() {
  const values = {
    lake_id: 'buntzen', timezone: 'UTC', release_time: '11:00',
    all_day_pass_url: 'https://example.test/custom-all', half_day_pass_url: 'https://example.test/custom-half',
    pass_priority_1: 'morning', pass_priority_2: '', pass_priority_3: 'all_day',
    name: 'Weekend plans', target_date: '2030-07-20', profile_id: '24', confirmation_mode: 'manual',
    prep_minutes_before: '40', csrf_token: 'synthetic-csrf',
  };
  const fields = Object.fromEntries(Object.entries(values).map(([name, value]) => {
    const field = new Element(name === 'lake_id' || name.startsWith('pass_priority_') ? 'select' : 'input');
    field.value = value;
    return [name, field];
  }));
  const definitions = [
    {id: 'buntzen', timezone: 'America/Vancouver', releaseTime: '07:00',
      allDayPassURL: 'https://example.test/buntzen-lake/All-Day-Pass', halfDayPassURL: 'https://example.test/buntzen-lake/Half-Day-Pass',
      releasePolicy: 'Buntzen Lake releases 1 day before the visit.',
      passes: [{value: 'all_day', label: 'All-day'}, {value: 'afternoon', label: 'Afternoon'}, {value: 'morning', label: 'Morning'}]},
    {id: 'test-lake', timezone: 'Etc/UTC', releaseTime: '09:30',
      allDayPassURL: 'https://example.test/test-lake/day', halfDayPassURL: '',
      releasePolicy: 'Test Lake releases 3 days before the visit.',
      passes: [{value: 'all_day', label: 'Day entry <test>'}]},
  ];
  for (const defaults of definitions) {
    const option = new Element('option');
    option.value = defaults.id;
    option.dataset.lakeDefaults = JSON.stringify(defaults);
    fields.lake_id.append(option);
  }
  const form = new Element('form');
  form.elements = {namedItem: name => fields[name] || null};
  const policy = new Element('p');
  policy.textContent = 'Existing booking release policy';
  return {form, policy, fields, definitions};
}

test('changing lakes replaces destination defaults and pass options while preserving trip choices', () => {
  const booking = bookingFixture();
  const page = openPage({liveJob: false, supportsEvents: false, booking});
  const fields = booking.fields;
  const preserved = ['name', 'target_date', 'profile_id', 'confirmation_mode', 'prep_minutes_before', 'csrf_token'];
  const original = Object.fromEntries(preserved.map(name => [name, fields[name].value]));
  fields.lake_id.value = 'test-lake';
  fields.lake_id.listeners.change();
  assert.equal(fields.timezone.value, 'Etc/UTC');
  assert.equal(fields.release_time.value, '09:30');
  assert.equal(fields.all_day_pass_url.value, 'https://example.test/test-lake/day');
  assert.equal(fields.half_day_pass_url.value, '', 'an absent URL must clear the previous lake URL');
  assert.equal(booking.policy.textContent, booking.definitions[1].releasePolicy);
  assert.equal(fields.pass_priority_1.value, 'all_day');
  assert.equal(fields.pass_priority_2.value, '');
  assert.equal(fields.pass_priority_3.value, '');
  for (const name of ['pass_priority_1', 'pass_priority_2', 'pass_priority_3']) {
    assert.deepEqual(fields[name].children.map(option => option.value), ['all_day', '']);
    assert.equal(fields[name].children[0].textContent, 'Day entry <test>');
    assert.equal(fields[name].children[0].children.length, 0, 'pass labels must remain text');
  }
  assert.deepEqual(Object.fromEntries(preserved.map(name => [name, fields[name].value])), original);
  fields.lake_id.value = 'buntzen';
  fields.lake_id.listeners.change();
  assert.equal(fields.timezone.value, 'America/Vancouver');
  assert.equal(fields.release_time.value, '07:00');
  assert.equal(fields.half_day_pass_url.value, booking.definitions[0].halfDayPassURL);
  assert.deepEqual(['pass_priority_1', 'pass_priority_2', 'pass_priority_3'].map(name => fields[name].value), ['all_day', 'afternoon', 'morning']);
  assert.equal(page.requests.length, 0);
  assert.equal(page.connections, 0);
});

test('opening an existing booking and reselecting its lake preserves custom settings', () => {
  const booking = bookingFixture();
  const original = Object.fromEntries(Object.entries(booking.fields).map(([name, field]) => [name, field.value]));
  const page = openPage({liveJob: false, booking});
  booking.fields.lake_id.listeners.change();
  assert.deepEqual(Object.fromEntries(Object.entries(booking.fields).map(([name, field]) => [name, field.value])), original);
  assert.equal(booking.policy.textContent, 'Existing booking release policy');
  assert.equal(page.requests.length, 0);
});

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
