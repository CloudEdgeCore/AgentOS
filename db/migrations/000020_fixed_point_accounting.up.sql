-- 000020: every persisted monetary value is exact fixed-point microUSD.
-- Floating-point columns are removed after deterministic six-decimal backfill.

ALTER TABLE task_budget_ledgers
    ADD COLUMN reserved_cost_micro_usd bigint NOT NULL DEFAULT 0 CHECK (reserved_cost_micro_usd >= 0),
    ADD COLUMN consumed_cost_micro_usd bigint NOT NULL DEFAULT 0 CHECK (consumed_cost_micro_usd >= 0);
UPDATE task_budget_ledgers SET
    reserved_cost_micro_usd = round(reserved_cost_usd * 1000000)::bigint,
    consumed_cost_micro_usd = round(consumed_cost_usd * 1000000)::bigint;
ALTER TABLE task_budget_ledgers DROP COLUMN reserved_cost_usd, DROP COLUMN consumed_cost_usd;

ALTER TABLE task_budget_settlements ADD COLUMN cost_micro_usd bigint NOT NULL DEFAULT 0 CHECK (cost_micro_usd >= 0);
UPDATE task_budget_settlements SET cost_micro_usd = round(cost_usd * 1000000)::bigint;
ALTER TABLE task_budget_settlements DROP COLUMN cost_usd;

ALTER TABLE task_usage_reservations ADD COLUMN cost_micro_usd bigint NOT NULL DEFAULT 0 CHECK (cost_micro_usd >= 0);
UPDATE task_usage_reservations SET cost_micro_usd = round(cost_usd * 1000000)::bigint;
ALTER TABLE task_usage_reservations DROP COLUMN cost_usd;

ALTER TABLE tenant_quotas ADD COLUMN cost_micro_usd bigint NOT NULL DEFAULT 0 CHECK (cost_micro_usd >= 0);
UPDATE tenant_quotas SET cost_micro_usd = round(cost_usd * 1000000)::bigint;
ALTER TABLE tenant_quotas DROP COLUMN cost_usd;

ALTER TABLE tenant_consumption_windows
    ADD COLUMN consumed_cost_micro_usd bigint NOT NULL DEFAULT 0 CHECK (consumed_cost_micro_usd >= 0),
    ADD COLUMN reserved_cost_micro_usd bigint NOT NULL DEFAULT 0 CHECK (reserved_cost_micro_usd >= 0);
UPDATE tenant_consumption_windows SET
    consumed_cost_micro_usd = round(consumed_cost_usd * 1000000)::bigint,
    reserved_cost_micro_usd = round(reserved_cost_usd * 1000000)::bigint;
ALTER TABLE tenant_consumption_windows DROP COLUMN consumed_cost_usd, DROP COLUMN reserved_cost_usd;

ALTER TABLE model_descriptors
    ADD COLUMN input_price_micro_usd_per_million bigint NOT NULL DEFAULT 0 CHECK (input_price_micro_usd_per_million >= 0),
    ADD COLUMN output_price_micro_usd_per_million bigint NOT NULL DEFAULT 0 CHECK (output_price_micro_usd_per_million >= 0);
UPDATE model_descriptors SET
    input_price_micro_usd_per_million = round(input_price_per_million * 1000000)::bigint,
    output_price_micro_usd_per_million = round(output_price_per_million * 1000000)::bigint;
ALTER TABLE model_descriptors DROP COLUMN input_price_per_million, DROP COLUMN output_price_per_million;

ALTER TABLE model_calls ADD COLUMN cost_micro_usd bigint NOT NULL DEFAULT 0 CHECK (cost_micro_usd >= 0);
UPDATE model_calls SET cost_micro_usd = round(cost_usd * 1000000)::bigint;
ALTER TABLE model_calls DROP COLUMN cost_usd;

ALTER TABLE workflows ADD COLUMN budget_max_cost_micro_usd bigint CHECK (budget_max_cost_micro_usd IS NULL OR budget_max_cost_micro_usd > 0);
UPDATE workflows SET budget_max_cost_micro_usd = round(budget_max_cost_usd * 1000000)::bigint
    WHERE budget_max_cost_usd IS NOT NULL;
ALTER TABLE workflows DROP COLUMN budget_max_cost_usd;
