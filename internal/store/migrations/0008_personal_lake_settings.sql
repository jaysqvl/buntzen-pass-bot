-- Personal defaults are separate from saved execution inputs. Existing
-- bookings retain Buntzen's original one-day release schedule.
ALTER TABLE profiles ADD COLUMN lake_id TEXT NOT NULL DEFAULT 'buntzen'
    CHECK (length(lake_id) BETWEEN 1 AND 64);
ALTER TABLE booking_requests ADD COLUMN release_days_before INTEGER NOT NULL DEFAULT 1
    CHECK (release_days_before BETWEEN 0 AND 365);

CREATE TABLE lake_settings (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    lake_id TEXT NOT NULL CHECK (length(lake_id) BETWEEN 1 AND 64),
    timezone TEXT NOT NULL,
    release_time TEXT NOT NULL,
    release_days_before INTEGER NOT NULL CHECK (release_days_before BETWEEN 0 AND 365),
    all_day_pass_url TEXT NOT NULL,
    half_day_pass_url TEXT NOT NULL,
    pass_order TEXT NOT NULL,
    prep_minutes_before INTEGER NOT NULL CHECK (prep_minutes_before BETWEEN 0 AND 180),
    auth_deadline_minutes_before INTEGER NOT NULL CHECK (auth_deadline_minutes_before BETWEEN 0 AND prep_minutes_before),
    poll_deadline_seconds INTEGER NOT NULL CHECK (poll_deadline_seconds BETWEEN 1 AND 900),
    poll_min_seconds REAL NOT NULL CHECK (poll_min_seconds BETWEEN 0.05 AND 60),
    poll_max_seconds REAL NOT NULL CHECK (poll_max_seconds BETWEEN poll_min_seconds AND 60),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, lake_id)
);

CREATE TABLE account_settings (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    headless INTEGER NOT NULL CHECK (headless IN (0, 1)),
    browser_channel TEXT NOT NULL CHECK (browser_channel IN ('', 'chrome', 'chrome-beta', 'chrome-dev', 'chrome-canary')),
    default_timeout_ms INTEGER NOT NULL CHECK (default_timeout_ms BETWEEN 1000 AND 120000),
    updated_at TEXT NOT NULL
);

-- Keep lake/profile association safe even for trusted non-HTTP writers.
-- A profile's credentials and browser state belong to its original lake.
CREATE TRIGGER profiles_lake_immutable
BEFORE UPDATE OF lake_id ON profiles
WHEN NEW.lake_id <> OLD.lake_id
BEGIN
    SELECT RAISE(ABORT, 'profile lake cannot be changed');
END;

CREATE TRIGGER booking_profile_lake_insert
BEFORE INSERT ON booking_requests
WHEN NOT EXISTS (
    SELECT 1 FROM profiles
    WHERE id = NEW.profile_id AND user_id = NEW.user_id AND lake_id = NEW.lake_id
)
BEGIN
    SELECT RAISE(ABORT, 'booking profile must belong to the selected lake');
END;

CREATE TRIGGER booking_profile_lake_update
BEFORE UPDATE OF profile_id, user_id, lake_id ON booking_requests
WHEN NOT EXISTS (
    SELECT 1 FROM profiles
    WHERE id = NEW.profile_id AND user_id = NEW.user_id AND lake_id = NEW.lake_id
)
BEGIN
    SELECT RAISE(ABORT, 'booking profile must belong to the selected lake');
END;
