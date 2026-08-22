package node

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type JoinAPI interface {
	IssueNodeJoinCredential(context.Context, string, string, client.JoinCredentialRequest) (client.JoinCredential, error)
	ConsumeNodeJoinCredential(context.Context, string, string, string, client.ConsumeJoinCredentialRequest) (client.Node, error)
}

type BrokerJoinCoordinator struct {
	API        JoinAPI
	State      StateStore
	Identities IdentityStore
	Now        func() time.Time

	mu      sync.Mutex
	pending *pendingJoinCredential
}

type pendingJoinCredential struct {
	PlanID     string
	IssuanceID string
	Identity   Identity
}

func NewBrokerJoinCoordinator(api JoinAPI, state StateStore, identities IdentityStore) (*BrokerJoinCoordinator, error) {
	if api == nil || state == nil || identities == nil {
		return nil, errors.New("broker join coordinator dependencies are incomplete")
	}
	return &BrokerJoinCoordinator{API: api, State: state, Identities: identities, Now: time.Now}, nil
}

func (c *BrokerJoinCoordinator) WorkerCredential(ctx context.Context, plan client.NodeInstallPlan) (RootJoinBinding, error) {
	state, identity, err := c.boundRuntime(plan)
	if err != nil {
		return RootJoinBinding{}, err
	}
	request := client.JoinCredentialRequest{
		EnrollmentID:             plan.EnrollmentID,
		PlanID:                   plan.PlanID,
		PlanDigest:               plan.Digest,
		NodeID:                   plan.NodeID,
		MachineFingerprint:       plan.Target.MachineFingerprint,
		NodePublicKeyFingerprint: state.Exchange.Identity.PublicKeyFingerprint,
	}
	proof, err := nodeProof(identity.PrivateKey, "blazn-node-join-v1", request)
	if err != nil {
		return RootJoinBinding{}, err
	}
	credential, err := c.API.IssueNodeJoinCredential(ctx, proof, "node-join-"+plan.PlanID, request)
	if err != nil {
		return RootJoinBinding{}, err
	}
	if err := client.ValidateJoinCredential(credential); err != nil {
		return RootJoinBinding{}, err
	}
	expiresAt, err := time.Parse(time.RFC3339, credential.ExpiresAt)
	if err != nil || !c.now().Before(expiresAt) || credential.ClusterID != plan.Cluster.ID || !credential.WorkerOnly {
		return RootJoinBinding{}, errors.New("broker join credential differs from the verified plan")
	}
	planExpiry, planErr := time.Parse(time.RFC3339, plan.ExpiresAt)
	identityExpiry, identityErr := time.Parse(time.RFC3339, state.Exchange.Identity.ExpiresAt)
	if planErr != nil || identityErr != nil || expiresAt.After(planExpiry) || expiresAt.After(identityExpiry) {
		return RootJoinBinding{}, errors.New("broker join credential exceeds the verified trust lifetime")
	}
	c.mu.Lock()
	c.pending = &pendingJoinCredential{PlanID: plan.PlanID, IssuanceID: credential.IssuanceID, Identity: identity}
	c.mu.Unlock()
	return RootJoinBinding{Credential: credential.Credential, ClusterID: credential.ClusterID, ExpectedNodeName: plan.Hostname, BootstrapTaint: plan.Cluster.BootstrapTaint, WorkerOnly: true}, nil
}

func (c *BrokerJoinCoordinator) ConfirmJoined(ctx context.Context, plan client.NodeInstallPlan, joined JoinedNode) error {
	if joined.Name != plan.Hostname || joined.UID == "" || joined.ResourceVersion == "" {
		return errors.New("joined node does not match the verified plan")
	}
	c.mu.Lock()
	pending := c.pending
	c.mu.Unlock()
	if pending == nil || pending.PlanID != plan.PlanID || pending.IssuanceID == "" {
		return errors.New("join credential issuance is unavailable for consumption")
	}
	request := client.ConsumeJoinCredentialRequest{NodeID: plan.NodeID, EnrollmentID: plan.EnrollmentID, PlanID: plan.PlanID, JoinedNodeUID: joined.UID, JoinedNodeName: joined.Name, ResourceVersion: joined.ResourceVersion, ClusterID: plan.Cluster.ID}
	proof, err := nodeProof(pending.Identity.PrivateKey, "blazn-node-join-v1", request)
	if err != nil {
		return err
	}
	node, err := c.API.ConsumeNodeJoinCredential(ctx, proof, pending.IssuanceID, "node-join-consume-"+pending.IssuanceID, request)
	if err != nil {
		return err
	}
	if err := client.ValidateNode(node); err != nil || node.ID != plan.NodeID || node.WorkspaceID != plan.WorkspaceID || node.KubernetesBinding == nil || node.KubernetesBinding.ClusterID != plan.Cluster.ID || node.KubernetesBinding.NodeName != joined.Name || node.KubernetesBinding.NodeUID != joined.UID || node.KubernetesBinding.ResourceVersion != joined.ResourceVersion {
		return errors.New("consumed join response differs from observed node binding")
	}
	c.mu.Lock()
	if c.pending == pending {
		c.pending = nil
	}
	c.mu.Unlock()
	return nil
}

func (c *BrokerJoinCoordinator) boundRuntime(plan client.NodeInstallPlan) (RuntimeState, Identity, error) {
	state, err := c.State.LoadRuntime()
	if err != nil {
		return RuntimeState{}, Identity{}, err
	}
	if state.Exchange.Plan.PlanID != plan.PlanID || state.Exchange.Plan.Digest != plan.Digest || state.Exchange.Plan.NodeID != plan.NodeID || state.Exchange.Plan.EnrollmentID != plan.EnrollmentID {
		return RuntimeState{}, Identity{}, errors.New("join plan differs from persisted verified runtime")
	}
	identity, err := c.Identities.LoadOrCreate()
	if err != nil {
		return RuntimeState{}, Identity{}, err
	}
	fingerprint, err := identity.Fingerprint()
	if err != nil || fingerprint != state.Exchange.Identity.PublicKeyFingerprint {
		return RuntimeState{}, Identity{}, errors.New("join identity differs from persisted verified runtime")
	}
	return state, identity, nil
}

func (c *BrokerJoinCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
