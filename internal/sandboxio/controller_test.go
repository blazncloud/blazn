package sandboxio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
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
	receipt, err := controller.Bootstrap(context.Background(), target, testManifest())
	if err != nil || len(receipt.Sources) != 1 || receipt.Sources[0].Commit != testManifest().Sources[0].Commit {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if owners.calls != 2 || !reflect.DeepEqual(transport.target, target) || !reflect.DeepEqual(transport.command, []string{HelperBinary, "bootstrap"}) {
		t.Fatalf("owners=%d target=%#v command=%v", owners.calls, transport.target, transport.command)
	}
	if err := controller.Release(context.Background(), target, receipt.Digest); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(transport.command, []string{HelperBinary, "release"}) {
		t.Fatalf("release command=%v", transport.command)
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

func TestControllerPreservesFramedHelperErrorThroughSuccessfulExec(t *testing.T) {
	fileSystem, err := OpenRootFileSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fileSystem.Close()
	controller, err := NewController(ControllerConfig{
		Transport: artifactServerTransport{fileSystem: fileSystem},
		Owners:    &ownerChecks{},
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := FrozenPodTarget{Namespace: "blazn-poc-sandboxes", PodName: "sandbox-a-pod", PodUID: "pod-uid-1", SandboxUID: "sandbox-uid-1", Container: ArtifactContainer}
	_, err = controller.ReadArtifact(context.Background(), target, "/workspace/artifacts/missing")
	if !IsProtocolError(err, "artifact_not_found") || strings.Contains(err.Error(), "transport failed") {
		t.Fatalf("framed helper error was not preserved: %v", err)
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
	state    *memoryBootstrapState
}

func (t *protocolTransport) Exec(ctx context.Context, target FrozenPodTarget, command []string, input io.Reader, output io.Writer) error {
	t.target, t.command = target, append([]string(nil), command...)
	var response bytes.Buffer
	if target.Container == BootstrapContainer {
		if t.state == nil {
			t.state = &memoryBootstrapState{}
		}
		if len(command) == 2 && command[1] == "release" {
			if err := ServeRelease(ctx, input, &response, t.state); err != nil {
				return err
			}
		} else if err := ServeBootstrap(ctx, input, &response, testMaterializer{}, t.state); err != nil {
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

type artifactServerTransport struct{ fileSystem ArtifactFileSystem }

func (t artifactServerTransport) Exec(ctx context.Context, _ FrozenPodTarget, _ []string, input io.Reader, output io.Writer) error {
	// ServeArtifact returning nil models the helper executable's exit status 0
	// after it successfully emits a logical error frame.
	return ServeArtifact(ctx, input, output, t.fileSystem)
}

func (*oversizeTransport) Exec(_ context.Context, _ FrozenPodTarget, _ []string, _ io.Reader, output io.Writer) error {
	_, _ = output.Write(make([]byte, MaxHeaderBytes+MaxManifestBytes+5))
	return nil
}

type stallTransport struct{}

func (stallTransport) Exec(ctx context.Context, _ FrozenPodTarget, _ []string, _ io.Reader, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}
