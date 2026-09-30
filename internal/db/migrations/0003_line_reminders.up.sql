CREATE TABLE contacts (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    line_user_id TEXT,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'blocked')),
    is_self      BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX contacts_line_user_idx ON contacts (user_id, line_user_id) WHERE line_user_id IS NOT NULL;
CREATE UNIQUE INDEX contacts_self_idx ON contacts (user_id) WHERE is_self;

-- Only a hash of each one-time code is stored.
CREATE TABLE binding_codes (
    id         BIGSERIAL PRIMARY KEY,
    contact_id BIGINT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    code_hash  BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);

CREATE TABLE reminder_rules (
    id           BIGSERIAL PRIMARY KEY,
    event_id     BIGINT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    lead_minutes INT NOT NULL CHECK (lead_minutes > 0),
    tips         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, lead_minutes)
);

-- One row per (event, recipient, lead time). The unique key is what prevents duplicate sends.
CREATE TABLE deliveries (
    id              BIGSERIAL PRIMARY KEY,
    event_id        BIGINT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    rule_id         BIGINT NOT NULL REFERENCES reminder_rules(id) ON DELETE CASCADE,
    contact_id      BIGINT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    lead_minutes    INT NOT NULL,
    due_at          TIMESTAMPTZ NOT NULL,
    late            BOOLEAN NOT NULL DEFAULT false,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'skipped')),
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    claimed_at      TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    sent_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, contact_id, lead_minutes)
);
CREATE INDEX deliveries_due_idx ON deliveries (next_attempt_at) WHERE status = 'pending';

-- Messages pushed per Taipei calendar month ("2026-09"), for the quota display in stage 4.
CREATE TABLE line_usage (
    month TEXT PRIMARY KEY,
    sent  INT NOT NULL DEFAULT 0
);
