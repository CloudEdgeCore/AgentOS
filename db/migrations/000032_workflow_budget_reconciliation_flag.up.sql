-- Workflow budget reconciliation flag closes the P1-05 gap left by 000028.
--
-- The 000028 upgrade backfill restored each undispatched step's task-count
-- slot but left its token/cost reservation at zero, because those ceilings
-- live inside the merged task spec and cannot be re-derived in pure SQL
-- (default task spec overlaid with the per-step overlay, exactly as the Go
-- dispatch path merges them). That leaves the workflow's committed token/cost
-- total understated for the pre-upgrade backlog, so a concurrent dynamic spawn
-- could slip past a ceiling the reservation was meant to defend.
--
-- This flag marks every workflow whose reservations may be understated. The
-- controller's ReconcileWorkflowReservations loop re-derives each undispatched
-- step's reservation through the same merge the dispatcher uses, resyncs the
-- ledger's step_reserved_* aggregate, and clears the flag. SpawnWorkflowStep
-- refuses new dynamic spawns while the flag is set, so the window is fail-safe
-- rather than over-committing.

ALTER TABLE workflows
    ADD COLUMN needs_budget_reconciliation boolean NOT NULL DEFAULT false;

-- Flag workflows that still hold undispatched steps: their token/cost
-- reservations are the ones 000028 could not reconstruct. Workflows created
-- after this migration reserve correctly at creation and default to false.
UPDATE workflows w SET needs_budget_reconciliation = true
WHERE EXISTS (
    SELECT 1 FROM workflow_steps s
    WHERE s.tenant_id = w.tenant_id AND s.workflow_id = w.id
      AND s.status IN ('PENDING', 'WAITING_APPROVAL')
);

-- The controller audits the flagged set oldest-first; a partial index keeps
-- that scan proportional to the reconcile backlog, not the whole table.
CREATE INDEX workflows_needs_budget_reconciliation_idx
    ON workflows (tenant_id, created_at)
    WHERE needs_budget_reconciliation;
