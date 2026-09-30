CREATE TABLE google_credentials (
    user_id           BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    refresh_token_enc BYTEA NOT NULL,
    calendar_id       TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'ok' CHECK (status IN ('ok', 'reauth_needed')),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE events (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_id      TEXT NOT NULL,
    google_event_id TEXT NOT NULL DEFAULT '',
    title           TEXT NOT NULL,
    start_at        TIMESTAMPTZ NOT NULL,
    duration_minutes INT NOT NULL,
    location        TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT 'other',
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, request_id)
);
CREATE INDEX events_user_start_idx ON events (user_id, start_at);
