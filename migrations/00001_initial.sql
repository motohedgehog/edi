-- +goose Up
CREATE TABLE profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    persona_id TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0),
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    all_day INTEGER NOT NULL DEFAULT 0 CHECK (all_day IN (0, 1)),
    schedule_mode TEXT NOT NULL DEFAULT 'custom'
        CHECK (schedule_mode IN ('peak', 'all_day', 'custom')),
    window_start TEXT CHECK (window_start IS NULL OR (length(window_start) = 5 AND substr(window_start, 3, 1) = ':' AND CAST(substr(window_start, 1, 2) AS INTEGER) BETWEEN 0 AND 23 AND CAST(substr(window_start, 4, 2) AS INTEGER) BETWEEN 0 AND 59)),
    window_end TEXT CHECK (window_end IS NULL OR (length(window_end) = 5 AND substr(window_end, 3, 1) = ':' AND CAST(substr(window_end, 1, 2) AS INTEGER) BETWEEN 0 AND 23 AND CAST(substr(window_end, 4, 2) AS INTEGER) BETWEEN 0 AND 59)),
    timezone TEXT NOT NULL,
    outage_notified INTEGER NOT NULL DEFAULT 0 CHECK (outage_notified IN (0, 1)),
    outage_generation INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (user_id, persona_id),
    CHECK (all_day = 1 OR (window_start IS NOT NULL AND window_end IS NOT NULL))
);

CREATE TABLE profile_lines (
    profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    line_id TEXT NOT NULL,
    PRIMARY KEY (profile_id, line_id)
);

CREATE TABLE monitor_states (
    profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    line_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    window_instance TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    good INTEGER NOT NULL CHECK (good IN (0, 1)),
    unknown INTEGER NOT NULL CHECK (unknown IN (0, 1)),
    severity_key TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL,
    reason TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    message_id INTEGER NOT NULL DEFAULT 0 CHECK (message_id >= 0),
    sent_at INTEGER,
    PRIMARY KEY (profile_id, line_id)
);

CREATE TABLE outbox_chunks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    line_ids TEXT NOT NULL,
    body TEXT NOT NULL,
    message_format TEXT NOT NULL DEFAULT 'plain' CHECK (message_format IN ('plain', 'html')),
    operation TEXT NOT NULL DEFAULT 'send' CHECK (operation IN ('send', 'edit')),
    message_id INTEGER NOT NULL DEFAULT 0 CHECK (message_id >= 0),
    version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    window_instance TEXT NOT NULL DEFAULT '',
    chunk_order INTEGER NOT NULL CHECK (chunk_order > 0),
    chunk_total INTEGER NOT NULL CHECK (chunk_total > 0),
    candidate_states TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    UNIQUE (profile_id, generation, event_type, line_ids, chunk_order)
);

CREATE TABLE catalog_lines (
    line_id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    mode_name TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE severity_metadata (
    mode_name TEXT NOT NULL,
    severity_level INTEGER NOT NULL,
    description TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (mode_name, severity_level)
);

CREATE TABLE reference_refresh (
    name TEXT PRIMARY KEY,
    attempted_at INTEGER NOT NULL DEFAULT 0,
    succeeded_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX outbox_pending_idx ON outbox_chunks (next_attempt_at, id);

-- +goose Down
DROP TABLE reference_refresh;
DROP TABLE severity_metadata;
DROP TABLE catalog_lines;
DROP TABLE outbox_chunks;
DROP TABLE monitor_states;
DROP TABLE profile_lines;
DROP TABLE profiles;
