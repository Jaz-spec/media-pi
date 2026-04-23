-- Phase 1 schema: recordings catalogue + upload queue + upload attempts audit trail
-- + a daemon heartbeat row for future TUI liveness indicators.
--
-- Events / commands tables arrive in later migrations.

CREATE TABLE recordings (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id        TEXT,                          -- platform event id; NULL for manual
  file_path       TEXT NOT NULL,
  status          TEXT NOT NULL,                 -- 'recording'|'stopped'|'failed'
  started_at      INTEGER NOT NULL,              -- unix seconds UTC
  stopped_at      INTEGER,
  stop_reason     TEXT,                          -- 'manual'|'scheduled_end'|'ffmpeg_died'|'operator_override'
  ffmpeg_pid      INTEGER,
  ffmpeg_log_path TEXT,
  notes           TEXT,
  created_at      INTEGER NOT NULL DEFAULT (strftime('%s','now')),
  updated_at      INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
CREATE INDEX idx_recordings_status ON recordings(status);
CREATE INDEX idx_recordings_event  ON recordings(event_id);

CREATE TABLE uploads (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  recording_id    INTEGER REFERENCES recordings(id),
  file_path       TEXT NOT NULL UNIQUE,
  status          TEXT NOT NULL,                 -- 'pending'|'uploading'|'uploaded'|'failed'
  attempt_count   INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT,
  last_log_path   TEXT,
  enqueued_at     INTEGER NOT NULL DEFAULT (strftime('%s','now')),
  started_at      INTEGER,
  finished_at     INTEGER
);
CREATE INDEX idx_uploads_status_enqueued ON uploads(status, enqueued_at);

CREATE TABLE upload_attempts (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  upload_id       INTEGER NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
  attempt_no      INTEGER NOT NULL,
  started_at      INTEGER NOT NULL,
  finished_at     INTEGER,
  exit_code       INTEGER,
  log_path        TEXT,
  error           TEXT
);
CREATE INDEX idx_upload_attempts_upload ON upload_attempts(upload_id);

CREATE TABLE daemon_heartbeat (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  last_beat_at    INTEGER NOT NULL,
  version         TEXT,
  pid             INTEGER
);
