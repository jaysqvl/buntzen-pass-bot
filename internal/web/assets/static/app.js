(() => {
  const root = document.getElementById('live-job');
  if (!root || !window.EventSource) return;
  const jobID = root.dataset.jobId;
  const csrf = root.dataset.csrf;
  const source = new EventSource(`/jobs/${encodeURIComponent(jobID)}/events`);
  const clearSensitive = () => {
    const code = document.getElementById('otp-code');
    if (code) code.textContent = '';
    const otpPanel = document.getElementById('otp-panel');
    if (otpPanel) otpPanel.hidden = true;
    const candidates = document.getElementById('pairing-candidates');
    if (candidates) candidates.replaceChildren();
    const pairingPanel = document.getElementById('pairing-panel');
    if (pairingPanel) pairingPanel.hidden = true;
  };
  source.addEventListener('auth_expired', () => {
    clearSensitive(); source.close(); window.location.replace('/login');
  });
  source.addEventListener('state', event => {
    const data = JSON.parse(event.data);
    document.getElementById('job-message').textContent = data.message || '';
    const pill = document.getElementById('job-pill');
    pill.textContent = data.label;
    pill.className = `pill ${data.class_name || ''}`;
    document.getElementById('approval-panel').hidden = !data.awaiting_approval;
    if (data.terminal) source.close();
  });
  source.addEventListener('otp', event => {
    const data = JSON.parse(event.data);
    const panel = document.getElementById('otp-panel');
    const code = document.getElementById('otp-code');
    if (data.active) { code.textContent = data.code; panel.hidden = false; }
    else { code.textContent = ''; panel.hidden = true; }
  });
  source.addEventListener('pairing', event => {
    const data = JSON.parse(event.data);
    const panel = document.getElementById('pairing-panel');
    const candidates = document.getElementById('pairing-candidates');
    candidates.replaceChildren();
    if (!data.active) { panel.hidden = true; return; }
    for (const candidate of data.candidates || []) {
      const button = document.createElement('button');
      button.type = 'button'; button.className = 'button';
      button.dataset.decision = 'pair'; button.dataset.messageId = candidate.id;
      button.textContent = `${candidate.code} · ${candidate.masked_sender} · ${candidate.service}`;
      candidates.append(button);
    }
    panel.hidden = false;
  });
  source.addEventListener('job_event', event => {
    const data = JSON.parse(event.data);
    const item = document.createElement('li');
    const time = document.createElement('time');
    time.textContent = data.time;
    const body = document.createElement('div');
    const title = document.createElement('strong');
    title.textContent = data.type;
    const text = document.createElement('p');
    text.textContent = data.message;
    body.append(title, text); item.append(time, body);
    document.getElementById('job-events').append(item);
  });
  root.addEventListener('click', async event => {
    const button = event.target.closest('[data-decision]');
    if (!button) return;
    button.disabled = true;
    const body = new URLSearchParams({csrf_token: csrf, decision: button.dataset.decision});
    if (button.dataset.messageId) body.set('message_id', button.dataset.messageId);
    const response = await fetch(`/jobs/${encodeURIComponent(jobID)}/decision`, {method:'POST', body, credentials:'same-origin', headers:{'Content-Type':'application/x-www-form-urlencoded'}});
    if (!response.ok) { button.disabled = false; alert(await response.text() || 'Request failed'); }
  });
  window.addEventListener('pagehide', () => {
    clearSensitive();
  });
})();
