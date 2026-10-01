-- "Disabled" contacts stay in the list but never receive reminders.
ALTER TABLE contacts ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT true;

-- Groups are only a convenience for ticking several recipients at once.
-- They have nothing to do with LINE group chats.
CREATE TABLE contact_groups (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

CREATE TABLE contact_group_members (
    group_id   BIGINT NOT NULL REFERENCES contact_groups(id) ON DELETE CASCADE,
    contact_id BIGINT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, contact_id)
);
