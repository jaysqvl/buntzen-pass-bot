-- Existing bookings retain their destination, schedules, and reservation keys.
-- Do not rewrite previous migrations or narrow the profile/date safeguards.
ALTER TABLE booking_requests ADD COLUMN lake_id TEXT NOT NULL DEFAULT 'buntzen'
    CHECK (length(lake_id) BETWEEN 1 AND 64);
