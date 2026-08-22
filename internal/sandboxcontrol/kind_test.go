package sandboxcontrol

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type noArtifactExporter struct{}

func (noArtifactExporter) Export(context.Context, SandboxRecord, []ArtifactExport) ([]ArtifactReceipt, error) {
	return []ArtifactReceipt{}, nil
}

func TestDisposableKindLifecycle(t *testing.T) {
	baseURL := os.Getenv("BLAZN_SANDBOX_KIND_PROXY_URL")
	image := os.Getenv("BLAZN_SANDBOX_KIND_IMAGE")
	if baseURL == "" || image == "" {
		t.Skip("disposable kind adapter endpoint is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adapter, err := New(Config{BaseURL: baseURL, HTTPClient: &http.Client{}, RuntimeClasses: map[string]RuntimeCapability{}, Exporter: noArtifactExporter{}, WatchIdleTimeout: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	name := "adapter-" + strings.ToLower(os.Getenv("BLAZN_SANDBOX_KIND_SUFFIX"))
	request := CreateRequest{
		RequestID: "kind-create-" + name, Name: name, WorkspaceID: "workspace-kind", OwnerID: "owner-kind", Image: image,
		Command: []string{"sh", "-c", "trap : TERM INT; sleep 3600 & wait"}, Architecture: "amd64",
		TrustLevel: TrustApprovedPOC, NonSensitive: true, CPURequest: "100m", MemoryRequest: "64Mi", CPULimit: "200m", MemoryLimit: "128Mi", ExpiresAt: time.Now().Add(time.Hour),
	}
	record, receipt, err := adapter.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	if record.QueueName != QueueName || record.WorkspaceID != request.WorkspaceID || record.OwnerID != request.OwnerID {
		t.Fatalf("created sandbox escaped adapter boundary: %#v", record)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		current, getErr := adapter.Get(cleanupCtx, request.WorkspaceID, request.OwnerID, name)
		if getErr != nil {
			return
		}
		if !current.Deleting {
			_, _ = adapter.Delete(cleanupCtx, "kind-cleanup-delete-"+name, request.WorkspaceID, request.OwnerID, name, current.UID, current.ResourceVersion)
			current, _ = adapter.Get(cleanupCtx, request.WorkspaceID, request.OwnerID, name)
		}
		if current.Deleting && contains(current.Finalizers, CleanupFinalizer) {
			_, _ = adapter.Finalize(cleanupCtx, "kind-cleanup-final-"+name, request.WorkspaceID, request.OwnerID, name, current.UID, current.ResourceVersion)
		}
	})
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, statusErr := adapter.Status(ctx, request.WorkspaceID, request.OwnerID, name)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.Ready && status.State == StateReady {
			break
		}
		if status.State == StateFailed || time.Now().After(deadline) {
			t.Fatalf("sandbox did not become ready: %#v", status)
		}
		time.Sleep(2 * time.Second)
	}
	list, err := adapter.List(ctx, request.WorkspaceID, request.OwnerID, "", 10)
	if err != nil || len(list.Sandboxes) != 1 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	events, eventErrors, err := adapter.Watch(ctx, request.WorkspaceID, request.OwnerID, list.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	current, err := adapter.Get(ctx, request.WorkspaceID, request.OwnerID, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Delete(ctx, "kind-delete-"+name, request.WorkspaceID, request.OwnerID, name, current.UID, current.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Sandbox.Name != name || event.Type != "MODIFIED" && event.Type != "DELETED" {
			t.Fatalf("delete watch event=%#v", event)
		}
	case watchErr := <-eventErrors:
		t.Fatalf("watch failed: %v", watchErr)
	case <-time.After(30 * time.Second):
		t.Fatal("delete watch event timed out")
	}
	deleting, err := adapter.Get(ctx, request.WorkspaceID, request.OwnerID, name)
	if err != nil {
		t.Fatal(err)
	}
	finalReceipt, err := adapter.Finalize(ctx, "kind-finalize-"+name, request.WorkspaceID, request.OwnerID, name, deleting.UID, deleting.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReceipt(finalReceipt); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		_, err = adapter.Get(ctx, request.WorkspaceID, request.OwnerID, name)
		var adapterErr *AdapterError
		if errorsAs(err, &adapterErr) && adapterErr.Code == ErrNotFound {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("sandbox remained after finalizer removal")
}

func errorsAs(err error, target **AdapterError) bool {
	for err != nil {
		if typed, ok := err.(*AdapterError); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		unwrapped, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}
