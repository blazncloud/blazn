package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

type retiringAPI struct {
	*mockAPI
	proof   string
	key     string
	request client.NodeRetirementRequest
	node    client.Node
	err     error
	calls   int
}

func (r *retiringAPI) RetireNode(_ context.Context, proof, key string, request client.NodeRetirementRequest) (client.Node, error) {
	r.calls++
	r.proof, r.key, r.request = proof, key, request
	return r.node, r.err
}

func TestRetireRemovedNodeSignsReceiptAndClassifiesFailures(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{PublicKey: publicKey, PrivateKey: privateKey}
	receipt := client.NodeInstallReceipt{ReceiptID: "77777777-7777-4777-8777-777777777777", NodeID: "11111111-1111-4111-8111-111111111111", State: "removed"}
	api := &retiringAPI{mockAPI: &mockAPI{}, node: client.Node{ID: receipt.NodeID, LifecycleState: "removed"}}
	runtime := &CommandRuntime{Service: NewService(api, fixedIdentity{identity}, nil, nil), Identities: fixedIdentity{identity}}

	if err := runtime.retireRemovedNode(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	if api.key != "node-retire-"+receipt.ReceiptID || api.request.Receipt.ReceiptID != receipt.ReceiptID {
		t.Fatalf("retirement key=%q request=%#v", api.key, api.request)
	}
	canonical, err := canonicalJSON(api.request)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(api.proof)
	if err != nil || !ed25519.Verify(publicKey, append([]byte("blazn-node-retirement-v1\n"), canonical...), signature) {
		t.Fatal("retirement proof invalid")
	}

	api.node = client.Node{ID: receipt.NodeID, LifecycleState: "active"}
	if err := runtime.retireRemovedNode(context.Background(), receipt); err == nil {
		t.Fatal("a response that is not removed must fail")
	}

	api.err = errors.New("call node API: connection refused")
	if err := runtime.retireRemovedNode(context.Background(), receipt); err == nil {
		t.Fatal("a transport failure must keep the cleanup journal for retry")
	}
	api.err = &client.APIError{StatusCode: 503, Body: client.ErrorBody{Code: "unavailable"}}
	if err := runtime.retireRemovedNode(context.Background(), receipt); err == nil {
		t.Fatal("a server failure must keep the cleanup journal for retry")
	}
	for _, code := range []string{"identity_rejected", "state_conflict"} {
		api.err = &client.APIError{StatusCode: 409, Body: client.ErrorBody{Code: code}}
		if err := runtime.retireRemovedNode(context.Background(), receipt); err != nil {
			t.Fatalf("definitive rejection %s must not block local cleanup: %v", code, err)
		}
	}

	calls := api.calls
	if err := (&CommandRuntime{}).retireRemovedNode(context.Background(), receipt); err != nil || api.calls != calls {
		t.Fatalf("runtime without a control-plane service must skip retirement: %v", err)
	}
}
