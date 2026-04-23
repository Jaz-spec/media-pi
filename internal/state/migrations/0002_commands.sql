-- Command bus between TUI and daemon. The TUI inserts rows; the daemon
-- consumes them, updates status, and optionally writes a result blob.
--
-- This is the SQLite-as-RPC mechanism: durable, inspectable, no network
-- surface. At ~1 cmd/sec peak the overhead is invisible.

CREATE TABLE commands (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  kind          TEXT NOT NULL,    -- 'start_recording'|'stop_recording'|'retry_upload'|'dismiss_failed'|'resolve_interlock'
  payload       TEXT,             -- JSON blob; kind-dependent shape
  status        TEXT NOT NULL DEFAULT 'pending', -- 'pending'|'accepted'|'done'|'error'
  result        TEXT,             -- JSON blob written by the daemon
  created_at    INTEGER NOT NULL DEFAULT (strftime('%s','now')),
  processed_at  INTEGER
);
CREATE INDEX idx_commands_status ON commands(status, id);
