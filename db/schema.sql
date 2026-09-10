CREATE TABLE checkpoint (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    cursor INTEGER NOT NULL,
    anchor TEXT NOT NULL,
    source TEXT NOT NULL,
    boundary INTEGER,
    generation INTEGER NOT NULL DEFAULT 0,
    expired INTEGER NOT NULL DEFAULT 0,
    last_expired INTEGER NOT NULL DEFAULT 0,
    last_generation INTEGER NOT NULL DEFAULT 0,
    last_boundary INTEGER,
    drain_discovered INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE pending (
    path TEXT PRIMARY KEY NOT NULL,
    source_id INTEGER NOT NULL,
    source TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    first_fail INTEGER NOT NULL DEFAULT 0,
    next_at INTEGER NOT NULL DEFAULT 0,
    final_generation INTEGER NOT NULL DEFAULT 0,
    expired INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX pending_fresh ON pending (source_id, path) WHERE attempts = 0 AND expired = 0;
CREATE INDEX pending_due ON pending (next_at, source_id, path) WHERE attempts > 0 AND expired = 0;
CREATE INDEX pending_final ON pending (final_generation, source_id, path) WHERE expired = 0;
