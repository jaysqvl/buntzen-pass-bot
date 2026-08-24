CREATE TABLE admins (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    singleton INTEGER NOT NULL DEFAULT 1 UNIQUE CHECK (singleton = 1),
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    csrf_token_hash TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions(expires_at);

CREATE TABLE login_attempts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rate_key TEXT NOT NULL,
    succeeded INTEGER NOT NULL CHECK (succeeded IN (0, 1)),
    attempted_at TEXT NOT NULL
);
CREATE INDEX login_attempts_lookup_idx ON login_attempts(rate_key, attempted_at);

CREATE TABLE otp_sources (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL CHECK (provider IN ('bluebubbles', 'twilio')),
    identity TEXT NOT NULL,
    config_ciphertext TEXT NOT NULL,
    pairing_chat_guid TEXT NOT NULL DEFAULT '',
    pairing_sender TEXT NOT NULL DEFAULT '',
    pairing_service TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(provider, identity)
);

CREATE TABLE profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    browser_profile TEXT NOT NULL COLLATE NOCASE UNIQUE,
    default_vehicle TEXT NOT NULL,
    otp_source_id INTEGER NOT NULL UNIQUE REFERENCES otp_sources(id) ON DELETE RESTRICT,
    yodel_email_ciphertext TEXT NOT NULL,
    yodel_password_ciphertext TEXT NOT NULL,
    headless INTEGER NOT NULL DEFAULT 1 CHECK (headless IN (0, 1)),
    browser_channel TEXT NOT NULL DEFAULT '',
    browser_executable TEXT NOT NULL DEFAULT '',
    default_timeout_ms INTEGER NOT NULL DEFAULT 15000 CHECK (default_timeout_ms > 0),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE booking_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    schedule_enabled INTEGER NOT NULL DEFAULT 0 CHECK (schedule_enabled IN (0, 1)),
    target_date TEXT NOT NULL,
    timezone TEXT NOT NULL DEFAULT 'UTC',
    release_time TEXT NOT NULL DEFAULT '07:00',
    prep_minutes_before INTEGER NOT NULL DEFAULT 30 CHECK (prep_minutes_before >= 0),
    auth_deadline_minutes_before INTEGER NOT NULL DEFAULT 5 CHECK (auth_deadline_minutes_before >= 0),
    poll_deadline_seconds INTEGER NOT NULL DEFAULT 120 CHECK (poll_deadline_seconds > 0),
    poll_min_seconds REAL NOT NULL DEFAULT 1.4 CHECK (poll_min_seconds > 0),
    poll_max_seconds REAL NOT NULL DEFAULT 3.6 CHECK (poll_max_seconds >= poll_min_seconds),
    confirmation_mode TEXT NOT NULL DEFAULT 'manual' CHECK (confirmation_mode IN ('manual', 'auto')),
    login_probe_url TEXT NOT NULL,
    all_day_pass_url TEXT NOT NULL DEFAULT '',
    half_day_pass_url TEXT NOT NULL DEFAULT '',
    check_all_day INTEGER NOT NULL DEFAULT 1 CHECK (check_all_day IN (0, 1)),
    check_afternoon INTEGER NOT NULL DEFAULT 0 CHECK (check_afternoon IN (0, 1)),
    check_morning INTEGER NOT NULL DEFAULT 0 CHECK (check_morning IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (auth_deadline_minutes_before <= prep_minutes_before),
    CHECK (check_all_day = 1 OR check_afternoon = 1 OR check_morning = 1)
);

CREATE TABLE jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    booking_request_id INTEGER REFERENCES booking_requests(id) ON DELETE RESTRICT,
    profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT,
    otp_source_id INTEGER NOT NULL REFERENCES otp_sources(id) ON DELETE RESTRICT,
    command TEXT NOT NULL CHECK (command IN ('auth-check', 'dry-run', 'book')),
    run_mode TEXT NOT NULL CHECK (run_mode IN ('dry-run', 'manual', 'auto')),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN (
        'queued', 'running', 'awaiting_approval', 'succeeded', 'failed',
        'cancelled', 'interrupted', 'outcome_unknown'
    )),
    due_at TEXT NOT NULL,
    expires_at TEXT,
    dedup_key TEXT NOT NULL DEFAULT '',
    owner TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
    message TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    confirmation_started_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);
CREATE UNIQUE INDEX jobs_dedup_idx ON jobs(dedup_key) WHERE dedup_key <> '';
CREATE UNIQUE INDEX jobs_one_active_profile_idx ON jobs(profile_id)
    WHERE status IN ('running', 'awaiting_approval');
CREATE UNIQUE INDEX jobs_one_active_source_idx ON jobs(otp_source_id)
    WHERE status IN ('running', 'awaiting_approval');
CREATE INDEX jobs_claim_idx ON jobs(status, due_at, id);
CREATE INDEX jobs_profile_idx ON jobs(profile_id, created_at);

CREATE TABLE job_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    level TEXT NOT NULL,
    kind TEXT NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    data_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX job_events_job_idx ON job_events(job_id, id);

CREATE TABLE job_decisions (
    job_id INTEGER PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    decision TEXT NOT NULL CHECK (decision IN ('approve', 'cancel')),
    created_at TEXT NOT NULL
);
