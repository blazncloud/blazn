package client

import (
	"strings"
	"testing"
)

func controlPlaneReceipt(operation NodeOperationType, outcome string, action NodeReceiptAction) NodeOperationReceipt {
	return NodeOperationReceipt{SchemaVersion: NodeSchemaVersion, ReceiptID: "11111111-1111-4111-8111-111111111111",
		OperationID: "22222222-2222-4222-8222-222222222222", NodeID: "33333333-3333-4333-8333-333333333333",
		WorkspaceID: "44444444-4444-4444-8444-444444444444", OperationType: operation, ExpectedNodeVersion: 4,
		StartedAt: "2026-10-01T12:00:00.000Z", CompletedAt: "2026-10-01T12:00:00.100Z", Outcome: outcome,
		Actions: []NodeReceiptAction{action}, Residues: []NodeReceiptResidue{}, SignerKind: "control_plane",
		SignerFingerprint: "sha256:" + strings.Repeat("ab", 32), SigningKeyID: "plan/v1",
		Digest: "sha256:" + strings.Repeat("cd", 32), Signature: strings.Repeat("A", 86)}
}

func TestControlPlaneReceiptMaySucceedOnlyForPlacementOperations(t *testing.T) {
	applied := NodeReceiptAction{Ordinal: 1, Kind: "api", Target: "node.lifecycleState=paused", Outcome: "applied"}
	for _, operation := range []NodeOperationType{"pause", "quarantine", "resume"} {
		if err := ValidateNodeOperationReceipt(controlPlaneReceipt(operation, "succeeded", applied)); err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	}
	if err := ValidateNodeOperationReceipt(controlPlaneReceipt("cordon", "succeeded", applied)); err == nil {
		t.Fatal("a control-plane receipt claimed success for a node-side operation")
	}
	host := NodeReceiptAction{Ordinal: 1, Kind: "service", Target: "blazn-node.service", Outcome: "applied"}
	if err := ValidateNodeOperationReceipt(controlPlaneReceipt("pause", "succeeded", host)); err == nil {
		t.Fatal("a control-plane placement receipt claimed a host action")
	}
	generation := int64(1)
	withGeneration := controlPlaneReceipt("resume", "succeeded", applied)
	withGeneration.IdentityGeneration = &generation
	if err := ValidateNodeOperationReceipt(withGeneration); err == nil {
		t.Fatal("a control-plane receipt carried a node identity generation")
	}
}
