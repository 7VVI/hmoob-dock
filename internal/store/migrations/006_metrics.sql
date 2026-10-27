-- Schema of the minute aggregates of the container metrics.
--
-- A row is one container of one node in one minute. The primary key is the
-- object identity and the start of the minute, so a replayed batch updates
-- the same row instead of counting the samples twice. A minute without a
-- valid sample has no row: a gap of the observation is not an idle
-- container and must not be stored as a zero.
--
-- Every timestamp is stored as UTC milliseconds in an INTEGER column. The
-- average and the peak keep every value of the calculation as a JSON
-- document; the sample count describes how many valid samples the row
-- holds and the expected count describes how many samples a complete
-- minute would have.

CREATE TABLE IF NOT EXISTS metric_minutes (
    node_id        TEXT    NOT NULL,
    container_id   TEXT    NOT NULL,
    bucket_start   INTEGER NOT NULL,
    sample_count   INTEGER NOT NULL,
    expected_count INTEGER NOT NULL,
    average_json   TEXT    NOT NULL,
    peak_json      TEXT    NOT NULL,
    PRIMARY KEY (node_id, container_id, bucket_start)
);

-- The retention removes the history by time. The primary key starts with
-- the object identity, so it does not serve a delete that names only the
-- start of the minute.
CREATE INDEX IF NOT EXISTS metric_minutes_by_start ON metric_minutes(bucket_start);
