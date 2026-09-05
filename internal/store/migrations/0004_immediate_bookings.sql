-- Keep immediate operator requests distinct from release-window jobs across
-- queueing and process restarts. Existing jobs retain their release timing.
ALTER TABLE jobs ADD COLUMN run_immediately INTEGER NOT NULL DEFAULT 0
CHECK (
    run_immediately IN (0, 1)
    AND (
        run_immediately = 0
        OR (command = 'book' AND run_mode = 'manual' AND expires_at IS NOT NULL)
    )
);
