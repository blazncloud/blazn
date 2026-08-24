package sandboxcontroller

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

type fakeSourceNetwork struct{ calls []string }

func (f *fakeSourceNetwork) Prepare(context.Context, WorkItem, sandboxcontrol.AdmissionObservation) error {
	f.calls = append(f.calls, "prepare")
	return nil
}
func (f *fakeSourceNetwork) Restrict(context.Context, WorkItem, sandboxcontrol.AdmissionObservation, sandboxio.SourceMaterializationReceipt) error {
	f.calls = append(f.calls, "restrict")
	return nil
}

type sourceOwnerChecks struct{ targets []sandboxio.FrozenPodTarget }

func (o *sourceOwnerChecks) VerifyPodOwner(_ context.Context, target sandboxio.FrozenPodTarget) error {
	o.targets = append(o.targets, target)
	return nil
}

type sourceProtocolTransport struct {
	commands [][]string
	receipt  sandboxio.SourceMaterializationReceipt
}

func (t *sourceProtocolTransport) Exec(ctx context.Context, _ sandboxio.FrozenPodTarget, command []string, input io.Reader, output io.Writer) error {
	t.commands = append(t.commands, append([]string(nil), command...))
	if len(command) != 2 {
		return io.ErrUnexpectedEOF
	}
	switch command[1] {
	case "bootstrap":
		_, body, err := sandboxio.DecodeRequest(ctx, input, sandboxio.OperationBootstrap)
		if err != nil {
			return err
		}
		manifest, _, err := sandboxio.ValidateSourceManifest(body)
		if err != nil {
			return err
		}
		sources := make([]sandboxio.SourceMaterialization, len(manifest.Sources))
		for index, source := range manifest.Sources {
			sources[index] = sandboxio.SourceMaterialization{Name: source.Name, URL: source.URL, Destination: source.Destination,
				Commit: source.Commit, Tree: source.Commit, ContentDigest: "sha256:" + strings.Repeat("e", 64), Writable: source.Writable}
		}
		t.receipt, err = sandboxio.NewSourceMaterializationReceipt(manifest, sources)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(t.receipt)
		return sandboxio.EncodeResponse(output, sandboxio.SuccessHeader(sandboxio.OperationBootstrap, encoded), encoded)
	case "release":
		_, body, err := sandboxio.DecodeRequest(ctx, input, sandboxio.OperationRelease)
		if err != nil {
			return err
		}
		request, err := sandboxio.DecodeReleaseRequest(body)
		if err != nil || request.ReceiptDigest != t.receipt.Digest {
			return io.ErrUnexpectedEOF
		}
		release := sandboxio.ReleaseReceipt{SchemaVersion: sandboxio.SourceReceiptVersion, ReceiptDigest: request.ReceiptDigest, Released: true}
		encoded, _ := json.Marshal(release)
		return sandboxio.EncodeResponse(output, sandboxio.SuccessHeader(sandboxio.OperationRelease, encoded), encoded)
	default:
		return io.ErrUnexpectedEOF
	}
}

func TestKubernetesSourceRuntimePinsObservationReceiptNetworkAndRelease(t *testing.T) {
	item, state := createFixture(t)
	item.Sources = []Source{{Name: "repo", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/repo", Commit: strings.Repeat("a", 40)}}
	network, transport, owners := &fakeSourceNetwork{}, &sourceProtocolTransport{}, &sourceOwnerChecks{}
	ioController, err := sandboxio.NewController(sandboxio.ControllerConfig{Transport: transport, Owners: owners, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewKubernetesSourceRuntime(KubernetesSourceRuntimeConfig{Network: network, IO: ioController})
	if err != nil {
		t.Fatal(err)
	}
	observation := *state.AdmissionObservation
	if err := runtime.Prepare(context.Background(), item, observation); err != nil {
		t.Fatal(err)
	}
	receipt, err := runtime.Materialize(context.Background(), item, observation)
	if err != nil {
		if failure, ok := BackendFailure(err); ok {
			t.Fatalf("%v: %v", err, failure.Cause)
		}
		t.Fatal(err)
	}
	if err := runtime.Restrict(context.Background(), item, observation, receipt); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Release(context.Background(), item, observation, receipt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(network.calls, []string{"prepare", "restrict"}) ||
		!reflect.DeepEqual(transport.commands, [][]string{{sandboxio.HelperBinary, "bootstrap"}, {sandboxio.HelperBinary, "release"}}) || len(owners.targets) != 4 {
		t.Fatalf("network=%v commands=%v ownerChecks=%d", network.calls, transport.commands, len(owners.targets))
	}
	for _, target := range owners.targets {
		if target.PodUID != observation.Pod.UID || target.SandboxUID != observation.Sandbox.UID || target.Container != sandboxio.BootstrapContainer {
			t.Fatalf("target=%#v", target)
		}
	}

	tampered := receipt
	tampered.Digest = "sha256:" + strings.Repeat("f", 64)
	if err := runtime.Restrict(context.Background(), item, observation, tampered); err == nil {
		t.Fatal("tampered source receipt reached the network transition")
	}
}
