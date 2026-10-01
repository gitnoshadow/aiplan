-- Events deleted in Google Calendar are kept (for history) but marked cancelled.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_status_check;
ALTER TABLE events ADD CONSTRAINT events_status_check CHECK (status IN ('pending', 'active', 'cancelled'));

-- Lets the sync find "our" event from a Google event id in one lookup.
CREATE UNIQUE INDEX events_google_idx ON events (user_id, google_event_id) WHERE google_event_id <> '';

-- Incremental sync state: Google's syncToken says "give me only what changed since last time".
CREATE TABLE calendar_sync (
    user_id        BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    sync_token     TEXT NOT NULL DEFAULT '',
    last_synced_at TIMESTAMPTZ,
    last_error     TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
