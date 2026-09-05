-- A reservation belongs to the immutable profile/date being booked, independent
-- of the request's mutable settings, confirmation mode, or scheduler dedup key.
-- Keep successful/ambiguous reservations when ordinary job history is pruned.
CREATE TABLE booking_reservations (
    profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    target_date TEXT NOT NULL,
    job_id INTEGER UNIQUE REFERENCES jobs(id) ON DELETE SET NULL,
    PRIMARY KEY (profile_id, target_date)
);

-- Older jobs did not snapshot their date. Scheduled keys retain it; otherwise
-- use the linked request. Preserve the strongest existing outcome, then the
-- active worker, then the oldest queued job when legacy duplicates exist.
INSERT INTO booking_reservations(profile_id, target_date, job_id)
SELECT profile_id, target_date, id FROM (
    SELECT candidates.*, row_number() OVER (
        PARTITION BY profile_id, target_date ORDER BY priority, id
    ) AS reservation_rank FROM (
        SELECT jobs.id, jobs.profile_id,
            CASE WHEN jobs.dedup_key = 'booking:' || jobs.booking_request_id || ':' || substr(jobs.dedup_key, -10)
                THEN substr(jobs.dedup_key, -10) ELSE booking.target_date END AS target_date,
            CASE WHEN jobs.confirmation_started_at IS NOT NULL OR jobs.status IN ('succeeded', 'outcome_unknown') THEN 0
                 WHEN jobs.status IN ('running', 'awaiting_approval') THEN 1 ELSE 2 END AS priority
        FROM jobs JOIN booking_requests AS booking ON booking.id = jobs.booking_request_id
        WHERE jobs.command = 'book' AND (
            jobs.status IN ('queued', 'running', 'awaiting_approval', 'succeeded', 'outcome_unknown')
            OR jobs.confirmation_started_at IS NOT NULL
        )
    ) AS candidates
) AS ranked WHERE reservation_rank = 1;

-- Do not launch an extra legacy job after its sibling completes. Active workers
-- retain their leases until they observe cancellation and stop their browsers.
UPDATE jobs SET
    cancel_requested = 1,
    status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
    message = 'Cancelled duplicate booking for an already reserved profile and date.',
    finished_at = CASE WHEN status = 'queued' THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE finished_at END,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE command = 'book' AND status IN ('queued', 'running', 'awaiting_approval')
  AND NOT EXISTS (SELECT 1 FROM booking_reservations WHERE job_id = jobs.id);

-- Migration runs with foreign keys temporarily disabled. An existing retention
-- trigger may prune old terminal rows during those cancellations; retain their
-- reservations as detached guards just as ON DELETE SET NULL does at runtime.
UPDATE booking_reservations SET job_id = NULL
WHERE job_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM jobs WHERE id = job_id);

CREATE TRIGGER jobs_reserve_booking
AFTER INSERT ON jobs
WHEN NEW.command = 'book'
BEGIN
    INSERT INTO booking_reservations(profile_id, target_date, job_id)
    SELECT NEW.profile_id, target_date, NEW.id
    FROM booking_requests WHERE id = NEW.booking_request_id;
END;

-- A failed/cancelled/interrupted attempt is retryable only while no confirmation
-- may have happened. The scheduler's separate dedup key still prevents an
-- automatic retry loop, while an operator can explicitly queue another attempt.
CREATE TRIGGER jobs_release_unconfirmed_booking
AFTER UPDATE OF status ON jobs
WHEN NEW.command = 'book' AND NEW.confirmation_started_at IS NULL
    AND NEW.status IN ('failed', 'cancelled', 'interrupted')
BEGIN
    DELETE FROM booking_reservations WHERE job_id = NEW.id;
END;

-- Retention triggers can delete a just-finished row before the release trigger
-- observes it. Release eligible reservations before any such deletion as well.
CREATE TRIGGER jobs_release_deleted_unconfirmed_booking
BEFORE DELETE ON jobs
WHEN OLD.command = 'book' AND OLD.confirmation_started_at IS NULL
    AND OLD.status IN ('failed', 'cancelled', 'interrupted')
BEGIN
    DELETE FROM booking_reservations WHERE job_id = OLD.id;
END;
