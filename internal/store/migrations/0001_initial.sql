CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL CHECK (username = trim(username)),
    username_normalized TEXT NOT NULL UNIQUE CHECK (username_normalized = lower(username)),
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX users_one_admin_idx ON users(role) WHERE role = 'admin';
CREATE INDEX users_status_username_idx ON users(status, username_normalized);

-- The first setup account is the one permanent administrator. These guards
-- keep the invariant intact even if a future caller bypasses the store API.
CREATE TRIGGER users_protect_admin_delete
BEFORE DELETE ON users
WHEN OLD.role = 'admin'
BEGIN
    SELECT RAISE(ABORT, 'the permanent administrator cannot be deleted');
END;

CREATE TRIGGER users_protect_admin_update
BEFORE UPDATE OF role, status ON users
WHEN OLD.role = 'admin' AND (NEW.role <> 'admin' OR NEW.status <> 'active')
BEGIN
    SELECT RAISE(ABORT, 'the permanent administrator cannot be demoted or disabled');
END;

CREATE TRIGGER users_protect_member_promotion
BEFORE UPDATE OF role ON users
WHEN OLD.role <> 'admin' AND NEW.role = 'admin'
BEGIN
    SELECT RAISE(ABORT, 'the permanent administrator cannot be replaced');
END;

CREATE TRIGGER users_require_disabled_member_delete
BEFORE DELETE ON users
WHEN OLD.role = 'member' AND OLD.status <> 'disabled'
BEGIN
    SELECT RAISE(ABORT, 'member account must be disabled before deletion');
END;

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token_hash TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions(expires_at);
CREATE INDEX sessions_user_idx ON sessions(user_id);
-- Keep at most 8 live session rows per account. A new login evicts the least
-- recently seen sessions before its row is inserted.
CREATE TRIGGER sessions_user_limit
BEFORE INSERT ON sessions
BEGIN
    DELETE FROM sessions
    WHERE user_id = NEW.user_id AND id NOT IN (
        SELECT id FROM sessions WHERE user_id = NEW.user_id
        ORDER BY last_seen_at DESC, id DESC LIMIT 7
    );
END;

CREATE TABLE login_attempts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rate_key TEXT NOT NULL,
    succeeded INTEGER NOT NULL CHECK (succeeded IN (0, 1)),
    attempted_at TEXT NOT NULL
);
CREATE INDEX login_attempts_lookup_idx ON login_attempts(rate_key, attempted_at);

CREATE TABLE otp_sources (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    provider TEXT NOT NULL CHECK (provider IN ('bluebubbles', 'twilio')),
    identity TEXT NOT NULL CHECK (length(identity) BETWEEN 1 AND 2048),
    config_ciphertext TEXT NOT NULL CHECK (length(config_ciphertext) <= 8192),
    pairing_chat_guid TEXT NOT NULL DEFAULT '' CHECK (length(pairing_chat_guid) <= 1024),
    pairing_sender TEXT NOT NULL DEFAULT '' CHECK (length(pairing_sender) <= 320),
    pairing_service TEXT NOT NULL DEFAULT '' CHECK (length(pairing_service) <= 64),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(user_id, name),
    UNIQUE(provider, identity),
    UNIQUE(id, user_id)
);
CREATE INDEX otp_sources_user_name_idx ON otp_sources(user_id, name);
-- Per-user durable resource ceilings are intentionally fixed for this
-- personal deployment. Store constants and regression tests mirror them.
CREATE TRIGGER otp_sources_user_limit
BEFORE INSERT ON otp_sources
WHEN (SELECT count(*) FROM otp_sources WHERE user_id = NEW.user_id) >= 24
BEGIN
    SELECT RAISE(ABORT, 'per-user OTP source limit reached');
END;
-- BlueBubbles does not expose a stable, permission-scoped server UUID through
-- the allowlisted API. This release therefore permits one Messages inbox per
-- control plane instead of guessing that URL aliases are different inboxes.
CREATE UNIQUE INDEX otp_sources_single_bluebubbles_idx ON otp_sources(provider)
WHERE provider = 'bluebubbles';
CREATE UNIQUE INDEX otp_sources_bluebubbles_pairing_idx
ON otp_sources(lower(pairing_chat_guid), lower(pairing_sender), lower(pairing_service))
WHERE provider = 'bluebubbles'
  AND pairing_chat_guid <> '' AND pairing_sender <> '' AND pairing_service <> '';

CREATE TABLE profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    default_vehicle TEXT NOT NULL CHECK (length(default_vehicle) BETWEEN 1 AND 256),
    otp_source_id INTEGER NOT NULL UNIQUE,
    yodel_email_ciphertext TEXT NOT NULL CHECK (length(yodel_email_ciphertext) <= 4096),
    yodel_password_ciphertext TEXT NOT NULL CHECK (length(yodel_password_ciphertext) <= 4096),
    headless INTEGER NOT NULL DEFAULT 1 CHECK (headless IN (0, 1)),
    browser_channel TEXT NOT NULL DEFAULT '' CHECK (length(browser_channel) <= 64),
    browser_executable TEXT NOT NULL DEFAULT '' CHECK (length(browser_executable) <= 2048),
    default_timeout_ms INTEGER NOT NULL DEFAULT 15000 CHECK (default_timeout_ms > 0),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(user_id, name),
    UNIQUE(id, user_id),
    FOREIGN KEY (otp_source_id, user_id) REFERENCES otp_sources(id, user_id) ON DELETE RESTRICT
);
CREATE INDEX profiles_user_name_idx ON profiles(user_id, name);
CREATE TRIGGER profiles_user_limit
BEFORE INSERT ON profiles
WHEN (SELECT count(*) FROM profiles WHERE user_id = NEW.user_id) >= 16
BEGIN
    SELECT RAISE(ABORT, 'per-user profile limit reached');
END;

CREATE TABLE booking_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    profile_id INTEGER NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    schedule_enabled INTEGER NOT NULL DEFAULT 0 CHECK (schedule_enabled IN (0, 1)),
    target_date TEXT NOT NULL,
    timezone TEXT NOT NULL DEFAULT 'UTC' CHECK (length(timezone) BETWEEN 1 AND 128),
    release_time TEXT NOT NULL DEFAULT '07:00',
    prep_minutes_before INTEGER NOT NULL DEFAULT 30 CHECK (prep_minutes_before BETWEEN 0 AND 180),
    auth_deadline_minutes_before INTEGER NOT NULL DEFAULT 5 CHECK (auth_deadline_minutes_before BETWEEN 0 AND 180),
    poll_deadline_seconds INTEGER NOT NULL DEFAULT 120 CHECK (poll_deadline_seconds > 0),
    poll_min_seconds REAL NOT NULL DEFAULT 1.4 CHECK (poll_min_seconds > 0),
    poll_max_seconds REAL NOT NULL DEFAULT 3.6 CHECK (poll_max_seconds >= poll_min_seconds),
    confirmation_mode TEXT NOT NULL DEFAULT 'manual' CHECK (confirmation_mode IN ('manual', 'auto')),
    login_probe_url TEXT NOT NULL CHECK (length(login_probe_url) BETWEEN 1 AND 2048),
    all_day_pass_url TEXT NOT NULL DEFAULT '' CHECK (length(all_day_pass_url) <= 2048),
    half_day_pass_url TEXT NOT NULL DEFAULT '' CHECK (length(half_day_pass_url) <= 2048),
    check_all_day INTEGER NOT NULL DEFAULT 1 CHECK (check_all_day IN (0, 1)),
    check_afternoon INTEGER NOT NULL DEFAULT 0 CHECK (check_afternoon IN (0, 1)),
    check_morning INTEGER NOT NULL DEFAULT 0 CHECK (check_morning IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(user_id, name),
    UNIQUE(id, user_id),
    FOREIGN KEY (profile_id, user_id) REFERENCES profiles(id, user_id) ON DELETE RESTRICT,
    CHECK (auth_deadline_minutes_before <= prep_minutes_before),
    CHECK (check_all_day = 1 OR check_afternoon = 1 OR check_morning = 1)
);
CREATE INDEX booking_requests_user_name_idx ON booking_requests(user_id, name);
CREATE TRIGGER booking_requests_user_limit
BEFORE INSERT ON booking_requests
WHEN (SELECT count(*) FROM booking_requests WHERE user_id = NEW.user_id) >= 64
BEGIN
    SELECT RAISE(ABORT, 'per-user booking request limit reached');
END;

CREATE TABLE jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    booking_request_id INTEGER,
    profile_id INTEGER NOT NULL,
    otp_source_id INTEGER NOT NULL,
    command TEXT NOT NULL CHECK (command IN ('auth-check', 'dry-run', 'book')),
    run_mode TEXT NOT NULL CHECK (run_mode IN ('dry-run', 'manual', 'auto')),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN (
        'queued', 'running', 'awaiting_approval', 'succeeded', 'failed',
        'cancelled', 'interrupted', 'outcome_unknown'
    )),
    due_at TEXT NOT NULL,
    expires_at TEXT,
    dedup_key TEXT NOT NULL DEFAULT '',
    worker_owner TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
    message TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    confirmation_started_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    UNIQUE(id, user_id),
    FOREIGN KEY (booking_request_id, user_id) REFERENCES booking_requests(id, user_id) ON DELETE RESTRICT,
    FOREIGN KEY (profile_id, user_id) REFERENCES profiles(id, user_id) ON DELETE RESTRICT,
    FOREIGN KEY (otp_source_id, user_id) REFERENCES otp_sources(id, user_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX jobs_dedup_idx ON jobs(dedup_key) WHERE dedup_key <> '';
CREATE UNIQUE INDEX jobs_one_pending_booking_run_idx
    ON jobs(user_id, booking_request_id, command, run_mode)
    WHERE booking_request_id IS NOT NULL
      AND status IN ('queued', 'running', 'awaiting_approval');
CREATE UNIQUE INDEX jobs_one_active_profile_idx ON jobs(profile_id)
    WHERE status IN ('running', 'awaiting_approval');
CREATE UNIQUE INDEX jobs_one_active_source_idx ON jobs(otp_source_id)
    WHERE status IN ('running', 'awaiting_approval');
CREATE INDEX jobs_claim_idx ON jobs(status, due_at, id);
CREATE INDEX jobs_user_id_idx ON jobs(user_id, id);
CREATE INDEX jobs_user_created_idx ON jobs(user_id, created_at DESC);
CREATE INDEX jobs_user_pending_idx ON jobs(user_id)
    WHERE status IN ('queued', 'running', 'awaiting_approval');
CREATE TRIGGER jobs_user_total_limit
BEFORE INSERT ON jobs
WHEN (SELECT count(*) FROM jobs WHERE user_id = NEW.user_id) >= 200
BEGIN
    SELECT RAISE(ABORT, 'per-user retained job limit reached');
END;
-- Retain the newest 128 prunable terminal jobs per user. Active work,
-- outcome_unknown, and an unexpired scheduler dedup key are never selected.
CREATE TRIGGER jobs_terminal_history_limit
AFTER UPDATE OF status ON jobs
WHEN NEW.status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
BEGIN
    DELETE FROM jobs WHERE id IN (
        SELECT id FROM jobs
        WHERE user_id = NEW.user_id
          AND status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
          AND (
              dedup_key = '' OR dedup_key GLOB 'pairing:*' OR
              (expires_at IS NOT NULL AND expires_at <= NEW.updated_at)
          )
        -- The UPDATE caller still needs to read NEW after this trigger runs.
        -- Keep it first even when a long-running job has an older id, then
        -- retain the most recently updated terminal history.
        ORDER BY CASE WHEN id = NEW.id THEN 0 ELSE 1 END,
                 updated_at DESC, id DESC
        LIMIT -1 OFFSET 128
    );
END;

CREATE TABLE job_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    job_id INTEGER NOT NULL,
    level TEXT NOT NULL,
    kind TEXT NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    data_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    FOREIGN KEY (job_id, user_id) REFERENCES jobs(id, user_id) ON DELETE CASCADE
);
CREATE INDEX job_events_job_idx ON job_events(job_id, id);
CREATE INDEX job_events_user_idx ON job_events(user_id, id);
CREATE TRIGGER job_events_per_job_limit
BEFORE INSERT ON job_events
BEGIN
    DELETE FROM job_events
    WHERE job_id = NEW.job_id AND id NOT IN (
        SELECT id FROM job_events WHERE job_id = NEW.job_id
        ORDER BY id DESC LIMIT 255
    );
END;

CREATE TABLE job_decisions (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    job_id INTEGER PRIMARY KEY,
    decision TEXT NOT NULL CHECK (decision IN ('approve', 'cancel')),
    created_at TEXT NOT NULL,
    FOREIGN KEY (job_id, user_id) REFERENCES jobs(id, user_id) ON DELETE CASCADE
);
CREATE INDEX job_decisions_user_idx ON job_decisions(user_id, job_id);
