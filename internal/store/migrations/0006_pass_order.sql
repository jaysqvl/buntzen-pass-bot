-- There are only fifteen nonempty, duplicate-free orders of the three passes.
-- Keep the stored order authoritative while retaining the legacy flag columns.
ALTER TABLE booking_requests ADD COLUMN pass_order TEXT NOT NULL DEFAULT 'all_day'
CHECK (pass_order IN (
    'all_day', 'afternoon', 'morning',
    'all_day,afternoon', 'all_day,morning',
    'afternoon,all_day', 'afternoon,morning',
    'morning,all_day', 'morning,afternoon',
    'all_day,afternoon,morning', 'all_day,morning,afternoon',
    'afternoon,all_day,morning', 'afternoon,morning,all_day',
    'morning,all_day,afternoon', 'morning,afternoon,all_day'
));

UPDATE booking_requests SET pass_order = rtrim(
    CASE WHEN check_all_day = 1 THEN 'all_day,' ELSE '' END ||
    CASE WHEN check_afternoon = 1 THEN 'afternoon,' ELSE '' END ||
    CASE WHEN check_morning = 1 THEN 'morning,' ELSE '' END,
    ','
);
