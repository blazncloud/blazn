BEGIN;

-- M4.3: a node that is not active must not receive new sandboxes.
--
-- Pause, quarantine and resume are node operations the control plane
-- completes itself, with a control-plane signed receipt. Quarantine joins
-- the operation types.
ALTER TABLE node_operations DROP CONSTRAINT node_operations_type_check;
ALTER TABLE node_operations ADD CONSTRAINT node_operations_type_check
  CHECK (type IN ('pause', 'resume', 'quarantine', 'label', 'cordon', 'uncordon', 'rotate_identity', 'repair', 'update', 'drain', 'remove'));
ALTER TABLE node_operation_receipts DROP CONSTRAINT node_operation_receipts_operation_type_check;
ALTER TABLE node_operation_receipts ADD CONSTRAINT node_operation_receipts_operation_type_check
  CHECK (operation_type IN ('pause', 'resume', 'quarantine', 'label', 'cordon', 'uncordon', 'rotate_identity', 'repair', 'update', 'drain', 'remove'));

-- placement_hold records the hold the MicroK8s worker issuer last applied
-- to the node's Kubernetes Node as the taint blazn.dev/placement-hold=<reason>
-- (NULL: no hold). The API reconciles it towards the hold the node's state
-- requires: its lifecycle state when paused, quarantined or draining, and
-- offline when an active node's heartbeat has lapsed.
ALTER TABLE nodes ADD COLUMN placement_hold text
  CHECK (placement_hold IN ('paused', 'quarantined', 'draining', 'offline'));

COMMIT;
