DROP TABLE IF EXISTS calendar_sync;
DROP INDEX IF EXISTS events_google_idx;
UPDATE events SET status = 'active' WHERE status = 'cancelled';
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_status_check;
ALTER TABLE events ADD CONSTRAINT events_status_check CHECK (status IN ('pending', 'active'));
