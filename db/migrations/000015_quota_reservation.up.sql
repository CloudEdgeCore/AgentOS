-- v0.8 quota reservation (ADR-016 follow-up): admission reserves the task's
-- budget ceiling against the tenant's current window under the window row
-- lock, so concurrent admissions cannot collectively overshoot the limit the
-- way the v0.6 consumed-only gate could. The reservation is released when
-- the task reaches a terminal state; the ledger records which window it was
-- reserved in, so a task spanning window boundaries releases the right row.

ALTER TABLE tenant_consumption_windows
    ADD COLUMN reserved_tokens bigint NOT NULL DEFAULT 0 CHECK (reserved_tokens >= 0),
    ADD COLUMN reserved_cost_usd double precision NOT NULL DEFAULT 0 CHECK (reserved_cost_usd >= 0),
    ADD COLUMN reserved_tool_calls bigint NOT NULL DEFAULT 0 CHECK (reserved_tool_calls >= 0),
    ADD COLUMN reserved_wall_seconds bigint NOT NULL DEFAULT 0 CHECK (reserved_wall_seconds >= 0);

ALTER TABLE task_budget_ledgers
    ADD COLUMN quota_reserved_window_start timestamptz;
