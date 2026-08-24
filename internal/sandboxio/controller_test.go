package sandboxio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func TestControllerPinsTargetCommandAndChecksOwnerBeforeAndAfter(t *testing.T) {
	transport := &protocolTransport{}
	owners := &ownerChecks{}
	controller, err := NewController(ControllerConfig{Transport: transport, Owners: owners, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	target := FrozenPodTarget{Namespace: "blazn-poc-sandboxes", PodName: "sandbox-a-pod", PodUID: "pod-uid-1", SandboxUID: "sandbox-uid-1", Container: BootstrapContainer}
	manifest, err := controller.Bootstrap(context.Background(), target, testManifest())
	if err != nil || !reflect.DeepEqual(manifest, testManifest()) {
		t.Fatalf("manifest=%#v err=%v", manifest, err)
	}
	if owners.calls != 2 || !reflect.DeepEqual(transport.target, target) || !reflect.DeepEqual(transport.command, []string{HelperBinary, "bootstrap"}) {
		t.Fatalf("owners=%d target=%#v command=%v", owners.calls, transport.target, transport.command)
	}

	target.Container = ArtifactContainer
	transport.artifact = []byte("artifact")
	artifact, err := controller.ReadArtifact(context.Background(), target, "/workspace/artifacts/result")
	if err != nil || string(artifact.Body) != "artifact" || artifact.Size != 8 {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	if !reflect.DeepEqual(transport.command, []string{HelperBinary, "artifact"}) {
		t.Fatalf("command=%v", transport.command)
	}
}

func TestControllerRejectsOwnerDriftInjectionOversizeAndStall(t *testing.T) {
	target := FrozenPodTarget{Namespace: "blazn-poc-sandboxes", PodName: "sandbox-a-pod", PodUID: "pod-uid-1", SandboxUID: "sandbox-uid-1", Container: BootstrapContainer}
	tests := []struct {
		name      string
		transport ExecTransport
		owners    *ownerChecks
	}{
		{name: "pre owner", transport: &protocolTransport{}, owners: &ownerChecks{failAt: 1}},
		{name: "post owner", transport: &protocolTransport{}, owners: &ownerChecks{failAt: 2}},
		{name: "injection", transport: &protocolTransport{inject: true}, owners: &ownerChecks{}},
		{name: "oversize", transport: &oversizeTransport{}, owners: &ownerChecks{}},
		{name: "stall", transport: stallTransport{}, owners: &ownerChecks{}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			controller, err := NewController(ControllerConfig{Transport: testCase.transport, Owners: testCase.owners, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if testCase.name == "stall" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			if _, err := controller.Bootstrap(ctx, target, testManifest()); err == nil {
				t.Fatal("unsafe exchange succeeded")
			}
		})
	}
}

type ownerChecks struct {
	calls  int
	failAt int
}

func (o *ownerChecks) VerifyPodOwner(context.Context, FrozenPodTarget) error {
	o.calls++
	if o.calls == o.failAt {
		return errors.New("owner changed")
	}
	return nil
}

type protocolTransport struct {
	target   FrozenPodTarget
	command  []string
	inject   bool
	artifact []byte
}

func (t *protocolTransport) Exec(ctx context.Context, target FrozenPodTarget, command []string, input io.Reader, output io.Writer) error {
	t.target, t.command = target, append([]string(nil), command...)
	var response bytes.Buffer
	if target.Container == BootstrapContainer {
		if err := ServeBootstrap(ctx, input, &response, nil); err != nil {
			return err
		}
	} else {
		_, _, err := DecodeRequest(ctx, input, OperationArtifact)
		if err != nil {
			return err
		}
		if err := EncodeResponse(&response, SuccessHeader(OperationArtifact, t.artifact), t.artifact); err != nil {
			return err
		}
	}
	if t.inject {
		response.WriteByte(0)
	}
	_, err := io.Copy(output, &response)
	return err
}

type oversizeTransport struct{}

func (*oversizeTransport) Exec(_ context.Context, _ FrozenPodTarget, _ []string, _ io.Reader, output io.Writer) error {
	_, _ = output.Write(make([]byte, MaxHeaderBytes+MaxManifestBytes+5))
	return nil
}

type stallTransport struct{}

func (stallTransport) Exec(ctx context.Context, _ FrozenPodTarget, _ []string, _ io.Reader, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}
