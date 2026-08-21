-- O6 scheduling backoff (v0.6): when no runtime pool can place an admitted
-- task, the scheduler releases its claim immediately and defers the next
-- attempt with exponential backoff instead of pinning the claim until its
-- TTL. next_schedule_attempt_at gates scheduling-claim eligibility;
-- schedule_retry_count drives the exponential factor and resets on
-- successful placement. The deferral lives on the task (not the claim) so
-- every controller instance sees the same backoff.

ALTER TABLE tasks
    ADD COLUMN next_schedule_attempt_at timestamptz,
    ADD COLUMN schedule_retry_count bigint NOT NULL DEFAULT 0 CHECK (schedule_retry_count >= 0);

CREATE INDEX tasks_schedule_eligibility_idx
    ON tasks (phase, next_schedule_attempt_at)
    WHERE phase = 'ADMITTED';
