package microk8sissuer

import (
	"encoding/json"
	"os"
	"testing"
)

func TestDecodeRequestClosedAndTTLBounded(t *testing.T) {
	base := requestFixture()
	data, _ := json.Marshal(base)
	if _, err := DecodeRequest(data); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){func(v map[string]any) { v["extra"] = true }, func(v map[string]any) { v["ttlSeconds"] = 301 }, func(v map[string]any) { delete(v, "workerOnly") }, func(v map[string]any) { v["bootstrapTaint"] = "other" }} {
		var value map[string]any
		_ = json.Unmarshal(data, &value)
		mutate(value)
		bad, _ := json.Marshal(value)
		if _, err := DecodeRequest(bad); err == nil {
			t.Fatalf("invalid request accepted: %s", bad)
		}
	}
}
func TestDeterministicTokenBindsEveryDomainField(t *testing.T) {
	root := t.TempDir()
	_ = os.Chmod(root, 0700)
	service, _ := NewService(root, []byte("0123456789abcdef0123456789abcdef"), &fakeBackend{})
	base := requestFixture()
	token := service.token(base)
	if len(token) != 32 {
		t.Fatalf("token length %d", len(token))
	}
	variants := []Request{base, base, base}
	variants[0].ClusterID = "cluster-b"
	variants[1].ExpectedNodeName = "worker-b"
	variants[2].IssuanceID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	variants = append(variants, base)
	variants[3].TTLSeconds = 61
	for _, variant := range variants {
		if service.token(variant) == token {
			t.Fatal("binding did not change deterministic token")
		}
	}
}
