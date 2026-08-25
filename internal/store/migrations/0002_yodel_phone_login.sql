-- Yodel's current portal uses a passwordless North American mobile-number
-- login. Rebuild profiles so legacy email/password columns cannot accidentally
-- remain part of the runtime credential contract. Existing rows receive no
-- mobile credential, so an old email value can never be submitted to the live
-- phone form. Legacy profiles are disabled and must be explicitly updated
-- before another run.
PRAGMA defer_foreign_keys = ON;

CREATE TABLE profiles_phone_login (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    default_vehicle TEXT NOT NULL CHECK (length(default_vehicle) BETWEEN 1 AND 256),
    otp_source_id INTEGER NOT NULL UNIQUE,
    yodel_phone_ciphertext TEXT NOT NULL CHECK (length(yodel_phone_ciphertext) <= 4096),
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

INSERT INTO profiles_phone_login(
    id, user_id, name, default_vehicle, otp_source_id, yodel_phone_ciphertext,
    headless, browser_channel, browser_executable, default_timeout_ms, enabled,
    created_at, updated_at
)
SELECT
	 id, user_id, name, default_vehicle, otp_source_id, '',
	 headless, browser_channel, browser_executable, default_timeout_ms, 0,
    created_at, updated_at
FROM profiles;

DROP TRIGGER profiles_user_limit;
DROP INDEX profiles_user_name_idx;
DROP TABLE profiles;
ALTER TABLE profiles_phone_login RENAME TO profiles;

CREATE INDEX profiles_user_name_idx ON profiles(user_id, name);
CREATE TRIGGER profiles_user_limit
BEFORE INSERT ON profiles
WHEN (SELECT count(*) FROM profiles WHERE user_id = NEW.user_id) >= 16
BEGIN
    SELECT RAISE(ABORT, 'per-user profile limit reached');
END;
