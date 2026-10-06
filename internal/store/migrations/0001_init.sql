-- Signal Lab initial schema.
--
-- Idempotency: event_id is the primary key, and (device_id, sequence) is unique
-- when a sequence is supplied. Inserts use ON CONFLICT DO NOTHING, so replays and
-- retries cannot create second rows.

CREATE TABLE events (
    event_id       text             PRIMARY KEY,
    device_id      text             NOT NULL,
    site_id        text,
    schema_version integer          NOT NULL,
    event_time     timestamptz      NOT NULL,  -- source timestamp (UTC)
    received_at    timestamptz      NOT NULL,  -- server-assigned on ingest
    sequence       bigint,
    temperature_c  double precision NOT NULL,
    vibration_mm_s double precision NOT NULL
);

-- At most one row per (device, sequence); events without a sequence are exempt.
CREATE UNIQUE INDEX events_device_sequence_uq ON events (device_id, sequence) WHERE sequence IS NOT NULL;
-- Serve GET /api/v1/events with and without a device filter (keyset order: event_time, event_id).
CREATE INDEX events_device_time_idx ON events (device_id, event_time, event_id);
CREATE INDEX events_time_idx ON events (event_time, event_id);

CREATE TABLE alerts (
    alert_id   text             PRIMARY KEY,  -- "<event_id>:<rule>": one alert per event and rule
    event_id   text             NOT NULL REFERENCES events (event_id),
    device_id  text             NOT NULL,
    rule       text             NOT NULL,
    threshold  double precision NOT NULL,
    observed   double precision NOT NULL,
    event_time timestamptz      NOT NULL,
    created_at timestamptz      NOT NULL
);

CREATE INDEX alerts_device_time_idx ON alerts (device_id, event_time, alert_id);
CREATE INDEX alerts_time_idx ON alerts (event_time, alert_id);
