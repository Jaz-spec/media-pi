-- Cached schedule of upcoming workshop events from the platform API.
-- The poller upserts rows every 60s; the scheduler fires events whose
-- start_time has arrived. Pruning removes rows that disappear from two
-- consecutive poll responses (but only if still 'pending').

CREATE TABLE events (
  id              TEXT PRIMARY KEY,      -- platform-assigned id
  workshop_name   TEXT NOT NULL,
  start_time      INTEGER NOT NULL,      -- unix seconds UTC
  end_time        INTEGER NOT NULL,
  trigger_status  TEXT NOT NULL DEFAULT 'pending',
                                          -- 'pending'|'fired'|'skipped_manual'|'missed'|'cancelled'
  recording_id    INTEGER REFERENCES recordings(id),
  last_seen_at    INTEGER NOT NULL,      -- bumped on each poll
  created_at      INTEGER NOT NULL DEFAULT (strftime('%s','now')),
  updated_at      INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
CREATE INDEX idx_events_start ON events(start_time);
CREATE INDEX idx_events_trigger_status ON events(trigger_status);
