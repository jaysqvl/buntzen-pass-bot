-- Keep every sign-in identity and its browser-directory ID intact. Profiles
-- are provider identities shared across lakes; vehicles belong to requests.
DROP TRIGGER profiles_lake_immutable;
DROP TRIGGER booking_profile_lake_insert;
DROP TRIGGER booking_profile_lake_update;

CREATE TABLE profiles_sequence_v10 AS SELECT seq FROM sqlite_sequence WHERE name = 'profiles';
CREATE TABLE profiles_shared_v10 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    default_vehicle TEXT NOT NULL DEFAULT '' CHECK (length(default_vehicle) <= 256),
    otp_source_id INTEGER NOT NULL,
    yodel_phone_ciphertext TEXT NOT NULL CHECK (length(yodel_phone_ciphertext) <= 4096),
    headless INTEGER NOT NULL DEFAULT 1 CHECK (headless IN (0, 1)),
    browser_channel TEXT NOT NULL DEFAULT '' CHECK (length(browser_channel) <= 64),
    browser_executable TEXT NOT NULL DEFAULT '' CHECK (length(browser_executable) <= 2048),
    default_timeout_ms INTEGER NOT NULL DEFAULT 15000 CHECK (default_timeout_ms > 0),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    login_probe_url TEXT NOT NULL DEFAULT '',
    lake_id TEXT NOT NULL DEFAULT 'buntzen' CHECK (length(lake_id) BETWEEN 1 AND 64),
    provider_id TEXT NOT NULL DEFAULT 'yodel' CHECK (length(provider_id) BETWEEN 1 AND 64),
    UNIQUE(user_id, name),
    UNIQUE(id, user_id),
    FOREIGN KEY (otp_source_id, user_id) REFERENCES otp_sources(id, user_id) ON DELETE RESTRICT
);
INSERT INTO profiles_shared_v10 (
    id, user_id, name, default_vehicle, otp_source_id, yodel_phone_ciphertext,
    headless, browser_channel, browser_executable, default_timeout_ms, enabled,
    created_at, updated_at, login_probe_url, lake_id, provider_id
)
SELECT id, user_id, name, default_vehicle, otp_source_id, yodel_phone_ciphertext,
    headless, browser_channel, browser_executable, default_timeout_ms, enabled,
    created_at, updated_at, login_probe_url, lake_id, 'yodel'
FROM profiles;
DROP TRIGGER profiles_user_limit;
DROP INDEX profiles_user_name_idx;
DROP TABLE profiles;
ALTER TABLE profiles_shared_v10 RENAME TO profiles;
-- Preserve the high-water mark even when the highest profile was deleted.
INSERT INTO sqlite_sequence(name, seq)
SELECT 'profiles', seq FROM profiles_sequence_v10
WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'profiles');
UPDATE sqlite_sequence SET seq = max(seq, COALESCE((SELECT seq FROM profiles_sequence_v10), 0)) WHERE name = 'profiles';
DROP TABLE profiles_sequence_v10;
CREATE INDEX profiles_user_name_idx ON profiles(user_id, name);
CREATE TRIGGER profiles_user_limit
BEFORE INSERT ON profiles
WHEN (SELECT count(*) FROM profiles WHERE user_id = NEW.user_id) >= 16
BEGIN
    SELECT RAISE(ABORT, 'per-user profile limit reached');
END;

ALTER TABLE lake_settings ADD COLUMN vehicle_keyword TEXT NOT NULL DEFAULT '' CHECK (length(vehicle_keyword) <= 256);
-- Preserve an existing lake's vehicle only when the legacy identities agree.
-- A conflicting choice needs the user to select a vehicle explicitly.
UPDATE lake_settings SET vehicle_keyword = COALESCE((
    SELECT min(trim(profile.default_vehicle)) FROM profiles AS profile
    WHERE profile.user_id = lake_settings.user_id AND profile.lake_id = lake_settings.lake_id
      AND trim(profile.default_vehicle) <> ''
    HAVING count(DISTINCT trim(profile.default_vehicle)) = 1
), '');
ALTER TABLE booking_requests ADD COLUMN vehicle_keyword TEXT NOT NULL DEFAULT '' CHECK (length(vehicle_keyword) <= 256);
UPDATE booking_requests SET vehicle_keyword = COALESCE((
    SELECT default_vehicle FROM profiles WHERE id = booking_requests.profile_id AND user_id = booking_requests.user_id
), '');

CREATE TABLE user_otp_preferences (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    source_id INTEGER NOT NULL,
    FOREIGN KEY (source_id, user_id) REFERENCES otp_sources(id, user_id) ON DELETE CASCADE
);
INSERT INTO user_otp_preferences(user_id, source_id)
SELECT owner.id, COALESCE(
    (SELECT profile.otp_source_id FROM profiles AS profile WHERE profile.user_id = owner.id ORDER BY profile.id LIMIT 1),
    (SELECT source.id FROM otp_sources AS source WHERE source.user_id = owner.id ORDER BY source.id LIMIT 1)
)
FROM users AS owner
WHERE EXISTS (SELECT 1 FROM otp_sources AS source WHERE source.user_id = owner.id);

CREATE TRIGGER otp_sources_default_for_owner
AFTER INSERT ON otp_sources
BEGIN
    INSERT INTO user_otp_preferences(user_id, source_id) VALUES (NEW.user_id, NEW.id)
    ON CONFLICT(user_id) DO NOTHING;
END;
