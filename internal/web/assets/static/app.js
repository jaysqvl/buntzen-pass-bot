(() => {
  const notifications = document.getElementById('notifications');
  const enableDismiss = notification => {
    const button = notification.querySelector('[data-dismiss-notification]');
    button.addEventListener('click', () => notification.remove());
    button.hidden = false;
  };
  for (const notification of notifications.querySelectorAll('[data-notification]')) {
    enableDismiss(notification);
  }
  const showNotification = (message, kind = 'error') => {
    const notification = document.createElement('div');
    notification.className = `flash notification ${kind}`;
    notification.dataset.notification = '';
    notification.setAttribute('role', kind === 'error' ? 'alert' : 'status');
    notification.setAttribute('aria-atomic', 'true');
    const content = document.createElement('div');
    content.className = 'notification-content';
    const text = document.createElement('span');
    text.className = 'notification-message';
    text.textContent = message;
    content.append(text);
    const dismiss = document.createElement('button');
    dismiss.type = 'button';
    dismiss.className = 'notification-dismiss';
    dismiss.dataset.dismissNotification = '';
    dismiss.setAttribute('aria-label', 'Dismiss notification');
    dismiss.textContent = '×';
    notification.append(content, dismiss);
    enableDismiss(notification);
    notifications.append(notification);
  };

  const bookingForm = document.getElementById('booking-form');
  if (bookingForm) {
    bookingForm.addEventListener('invalid', event => {
      const section = event.target.closest('.form-advanced');
      if (section) section.open = true;
    }, true);
    const lakeSelector = bookingForm.elements.namedItem('lake_id');
    let selectedLake = lakeSelector.value;
    lakeSelector.addEventListener('change', () => {
      if (lakeSelector.value === selectedLake) return;
      const option = lakeSelector.selectedOptions[0];
      if (!option?.dataset.lakeDefaults) return;
      let defaults;
      try { defaults = JSON.parse(option.dataset.lakeDefaults); }
      catch { showNotification('Lake settings could not be loaded. Reload this page before saving.'); return; }
      if (defaults.id !== lakeSelector.value) return;
      for (const [name, value] of Object.entries({
        timezone: defaults.timezone, release_time: defaults.releaseTime,
        all_day_pass_url: defaults.allDayPassURL, half_day_pass_url: defaults.halfDayPassURL,
      })) {
        bookingForm.elements.namedItem(name).value = value;
      }
      for (let index = 0; index < 3; index++) {
        const field = bookingForm.elements.namedItem(`pass_priority_${index + 1}`);
        const choices = [...defaults.passes, {value: '', label: 'None'}].map(pass => {
          const choice = document.createElement('option');
          choice.value = pass.value; choice.textContent = pass.label;
          return choice;
        });
        field.replaceChildren(...choices);
        field.value = defaults.passes[index]?.value || '';
      }
      document.getElementById('lake-release-policy').textContent = defaults.releasePolicy;
      selectedLake = lakeSelector.value;
    });
  }

  const sourceForm = document.getElementById('source-form');
  if (sourceForm) {
    const providerSelector = sourceForm.elements.namedItem('provider');
    const providerSections = sourceForm.querySelectorAll('[data-source-provider]');
    const updateProviderSections = () => {
      for (const section of providerSections) {
        const inactive = section.dataset.sourceProvider !== providerSelector.value;
        section.hidden = inactive;
        section.disabled = inactive;
      }
    };
    providerSelector.addEventListener('change', updateProviderSections);
    updateProviderSections();
  }

  const root = document.getElementById('live-job');
  if (!root || !window.EventSource) return;
  const jobID = root.dataset.jobId;
  const csrf = root.dataset.csrf;
  const source = new EventSource(`/jobs/${encodeURIComponent(jobID)}/events?after=${encodeURIComponent(root.dataset.lastEventId || '0')}`);
  let terminal = false;
  let lastEventID = Number(root.dataset.lastEventId || 0);
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
  source.addEventListener('error', clearSensitive);
  source.addEventListener('complete', () => { clearSensitive(); source.close(); });
  source.addEventListener('state', event => {
    const data = JSON.parse(event.data);
    document.getElementById('job-message').textContent = data.message || '';
    const pill = document.getElementById('job-pill');
    pill.textContent = data.label;
    pill.className = `pill ${data.class_name || ''}`;
    document.getElementById('job-status').textContent = data.label;
    document.getElementById('job-started').textContent = data.started;
    document.getElementById('job-finished').textContent = data.finished;
    document.getElementById('job-confirmation').textContent = data.confirmation_started;
    const cancel = document.getElementById('cancel-job');
    cancel.hidden = !data.can_cancel;
    if (!data.can_cancel) cancel.disabled = true;
    document.getElementById('approval-panel').hidden = !data.awaiting_approval;
    terminal = data.terminal;
    if (terminal) clearSensitive();
  });
  source.addEventListener('otp', event => {
    if (terminal) return;
    const data = JSON.parse(event.data);
    const panel = document.getElementById('otp-panel');
    const code = document.getElementById('otp-code');
    if (data.active) { code.textContent = data.code; panel.hidden = false; }
    else { code.textContent = ''; panel.hidden = true; }
  });
  source.addEventListener('pairing', event => {
    if (terminal) return;
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
    if (data.id <= lastEventID) return;
    lastEventID = data.id;
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
    if (!button || terminal || button.disabled) return;
    button.disabled = true;
    const body = new URLSearchParams({csrf_token: csrf, decision: button.dataset.decision});
    if (button.dataset.messageId) body.set('message_id', button.dataset.messageId);
    try {
      const response = await fetch(`/jobs/${encodeURIComponent(jobID)}/decision`, {method:'POST', body, credentials:'same-origin', headers:{'Content-Type':'application/x-www-form-urlencoded'}});
      const contentType = (response.headers.get('Content-Type') || '').split(';')[0].trim().toLowerCase();
      if (response.redirected || contentType === 'text/html' || contentType === 'application/xhtml+xml') {
        button.disabled = terminal;
        clearSensitive();
        showNotification("The app couldn't confirm this action. Sign in or check Account, then check the job status before retrying.");
      } else if (response.status !== 204) {
        button.disabled = terminal;
        const message = !response.ok && contentType === 'text/plain' ? await response.text() : '';
        showNotification(message || "The app couldn't confirm this action. Check the job status before retrying.");
      }
    } catch {
      button.disabled = terminal;
      showNotification('Connection lost. Check the job status before retrying.');
    }
  });
  window.addEventListener('pagehide', () => {
    clearSensitive();
  });
})();
