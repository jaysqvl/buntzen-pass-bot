-- Preparation and retry policy is shared across an account's lakes.
-- Saved profiles, bookings, and queued jobs keep their own execution inputs.
ALTER TABLE account_settings ADD COLUMN prep_minutes_before INTEGER NOT NULL DEFAULT 30
    CHECK (prep_minutes_before BETWEEN 0 AND 180);
ALTER TABLE account_settings ADD COLUMN auth_deadline_minutes_before INTEGER NOT NULL DEFAULT 5
    CHECK (auth_deadline_minutes_before BETWEEN 0 AND prep_minutes_before);
ALTER TABLE account_settings ADD COLUMN poll_deadline_seconds INTEGER NOT NULL DEFAULT 120
    CHECK (poll_deadline_seconds BETWEEN 1 AND 900);
ALTER TABLE account_settings ADD COLUMN poll_min_seconds REAL NOT NULL DEFAULT 1.4
    CHECK (poll_min_seconds BETWEEN 0.05 AND 60);
ALTER TABLE account_settings ADD COLUMN poll_max_seconds REAL NOT NULL DEFAULT 3.6
    CHECK (poll_max_seconds BETWEEN poll_min_seconds AND 60);

-- Preserve the previously editable timing policy. Buntzen is the currently
-- supported lake; deterministic ordering also handles any extra stored IDs.
-- Existing account browser choices and their saved timestamp are unchanged.
INSERT INTO account_settings (
    user_id, headless, browser_channel, default_timeout_ms,
    prep_minutes_before, auth_deadline_minutes_before, poll_deadline_seconds,
    poll_min_seconds, poll_max_seconds, updated_at
)
SELECT source.user_id, 1, '', 15000,
    source.prep_minutes_before, source.auth_deadline_minutes_before,
    source.poll_deadline_seconds, source.poll_min_seconds, source.poll_max_seconds,
    source.updated_at
FROM lake_settings AS source
WHERE source.lake_id = (
    SELECT candidate.lake_id FROM lake_settings AS candidate
    WHERE candidate.user_id = source.user_id
    ORDER BY (candidate.lake_id = 'buntzen') DESC, candidate.lake_id
    LIMIT 1
)
ON CONFLICT(user_id) DO UPDATE SET
    prep_minutes_before = excluded.prep_minutes_before,
    auth_deadline_minutes_before = excluded.auth_deadline_minutes_before,
    poll_deadline_seconds = excluded.poll_deadline_seconds,
    poll_min_seconds = excluded.poll_min_seconds,
    poll_max_seconds = excluded.poll_max_seconds;

CREATE TABLE lake_settings_v9 (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    lake_id TEXT NOT NULL CHECK (length(lake_id) BETWEEN 1 AND 64),
    timezone TEXT NOT NULL,
    release_time TEXT NOT NULL,
    release_days_before INTEGER NOT NULL CHECK (release_days_before BETWEEN 0 AND 365),
    all_day_pass_url TEXT NOT NULL,
    half_day_pass_url TEXT NOT NULL,
    pass_order TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, lake_id)
);

INSERT INTO lake_settings_v9 (
    user_id, lake_id, timezone, release_time, release_days_before,
    all_day_pass_url, half_day_pass_url, pass_order, updated_at
)
SELECT user_id, lake_id, timezone, release_time, release_days_before,
    all_day_pass_url, half_day_pass_url, pass_order, updated_at
FROM lake_settings;

DROP TABLE lake_settings;
ALTER TABLE lake_settings_v9 RENAME TO lake_settings;
