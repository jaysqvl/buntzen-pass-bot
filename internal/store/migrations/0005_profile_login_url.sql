-- Authentication belongs to a profile, including profiles with no bookings.
-- Preserve the prior pairing selection order when several saved requests
-- provide login URLs. Keep legacy request values intact for history/review.
ALTER TABLE profiles ADD COLUMN login_probe_url TEXT NOT NULL DEFAULT '';

-- New pairing jobs no longer have a booking_request_id, so they need their
-- own admission guard against concurrent Pair clicks. Historical pairing jobs
-- keep their booking association and existing pending-booking uniqueness.
CREATE UNIQUE INDEX jobs_one_pending_profile_pairing_idx ON jobs(otp_source_id)
WHERE booking_request_id IS NULL AND command = 'auth-check'
  AND dedup_key GLOB 'pairing:*'
  AND status IN ('queued', 'running', 'awaiting_approval');

UPDATE profiles
SET login_probe_url = COALESCE((
    SELECT trim(booking.login_probe_url)
    FROM booking_requests AS booking
    WHERE booking.profile_id = profiles.id
      AND booking.user_id = profiles.user_id
      AND trim(booking.login_probe_url) <> ''
    ORDER BY booking.enabled DESC, booking.name, booking.id
    LIMIT 1
), '');
