ALTER TABLE workflow_steps ADD COLUMN approval_decision text
    CHECK (approval_decision IS NULL OR approval_decision IN ('approved','rejected'));

UPDATE workflow_steps SET approval_decision=decided_by, decided_by='legacy/unknown'
    WHERE decided_by IN ('approved','rejected');
