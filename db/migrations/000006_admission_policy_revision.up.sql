-- Every admission decision records the Rego policy revision that produced
-- it, so outcomes stay auditable across policy updates.
ALTER TABLE admission_decisions
    ADD COLUMN policy_revision text;
