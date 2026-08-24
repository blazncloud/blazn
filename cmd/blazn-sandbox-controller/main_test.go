package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxcontroller"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

type factoryStore struct{ closed bool }

func (*factoryStore) Claim(context.Context, string, int) (*sandboxcontroller.WorkItem, error) {
	return nil, nil
}
func (*factoryStore) Renew(context.Context, string, string, string, int) (sandboxcontroller.LeaseWindow, bool, error) {
	return sandboxcontroller.LeaseWindow{}, false, nil
}
func (*factoryStore) BindBackend(context.Context, string, string, string, sandboxcontrol.AdmissionObservation) (bool, error) {
	return false, nil
}
func (*factoryStore) RecordSources(context.Context, string, string, string, sandboxcontrol.AdmissionObservation, sandboxio.SourceMaterializationReceipt) (bool, error) {
	return false, nil
}
func (*factoryStore) Retry(context.Context, string, string, string, int, sandboxcontroller.SafeError) (sandboxcontroller.RetryOutcome, error) {
	return sandboxcontroller.Fenced, nil
}
func (*factoryStore) Complete(context.Context, string, string, string, sandboxcontroller.Completion) (bool, error) {
	return false, nil
}
func (*factoryStore) EnqueueExpired(context.Context, int) (int, error) { return 0, nil }
func (*factoryStore) Health(context.Context) error                     { return nil }
func (store *factoryStore) Close() error                               { store.closed = true; return nil }

type factoryBackend struct{}

func (*factoryBackend) Health(context.Context) error { return nil }
func (*factoryBackend) EnsureCreated(context.Context, sandboxcontroller.WorkItem) (sandboxcontroller.BackendState, error) {
	return sandboxcontroller.BackendState{}, nil
}
func (*factoryBackend) Observe(context.Context, sandboxcontroller.WorkItem, *sandboxcontrol.AdmissionObservation) (sandboxcontroller.BackendState, error) {
	return sandboxcontroller.BackendState{}, nil
}
func (*factoryBackend) BeginDelete(context.Context, sandboxcontroller.WorkItem, *sandboxcontrol.AdmissionObservation) (sandboxcontroller.BackendState, error) {
	return sandboxcontroller.BackendState{}, nil
}
func (*factoryBackend) Finalize(context.Context, sandboxcontroller.WorkItem, sandboxcontroller.BackendState, *sandboxcontrol.AdmissionObservation) (sandboxcontroller.CleanupResult, error) {
	return sandboxcontroller.CleanupResult{}, nil
}

type factoryRunner struct{ err error }

func (runner factoryRunner) Run(context.Context) error { return runner.err }

func TestRunWithConstructsValidatedBackendBeforeStoreAndClosesStore(t *testing.T) {
	getenv := controllerEnvironment(t)
	store, backend := &factoryStore{}, &factoryBackend{}
	var calls []string
	factories := runtimeFactories{
		newBackend: func(config sandboxcontroller.KubernetesConfig) (sandboxcontroller.Backend, error) {
			calls = append(calls, "backend")
			if config.BaseURL != "https://10.96.0.1:443" {
				t.Fatalf("unexpected Kubernetes config: %#v", config)
			}
			return backend, nil
		},
		openStore: func(databaseURL string) (sandboxcontroller.Store, error) {
			calls = append(calls, "store")
			if databaseURL != "postgres://controller@database/blazn" {
				t.Fatalf("unexpected database URL")
			}
			return store, nil
		},
		newController: func(gotStore sandboxcontroller.Store, gotBackend sandboxcontroller.Backend, _ sandboxcontroller.Config) (controllerRunner, error) {
			calls = append(calls, "controller")
			if gotStore != store || gotBackend != backend {
				t.Fatal("controller factory received partial dependencies")
			}
			return factoryRunner{}, nil
		},
	}
	if code := runWith(context.Background(), getenv, factories); code != 0 || !store.closed || len(calls) != 3 || calls[0] != "backend" || calls[1] != "store" || calls[2] != "controller" {
		t.Fatalf("runtime factory ordering failed: code=%d closed=%v calls=%v", code, store.closed, calls)
	}
}

func TestRunWithConfigurationFailureMakesZeroBackendOrStoreCalls(t *testing.T) {
	backendCalls, storeCalls := 0, 0
	factories := runtimeFactories{
		newBackend: func(sandboxcontroller.KubernetesConfig) (sandboxcontroller.Backend, error) {
			backendCalls++
			return &factoryBackend{}, nil
		},
		openStore: func(string) (sandboxcontroller.Store, error) { storeCalls++; return &factoryStore{}, nil },
		newController: func(sandboxcontroller.Store, sandboxcontroller.Backend, sandboxcontroller.Config) (controllerRunner, error) {
			return factoryRunner{}, nil
		},
	}
	if code := runWith(context.Background(), func(string) string { return "" }, factories); code != 2 || backendCalls != 0 || storeCalls != 0 {
		t.Fatalf("invalid configuration reached a factory: code=%d backend=%d store=%d", code, backendCalls, storeCalls)
	}
}

func TestRunWithBackendFailureDoesNotOpenDatabase(t *testing.T) {
	storeCalls := 0
	factories := runtimeFactories{
		newBackend: func(sandboxcontroller.KubernetesConfig) (sandboxcontroller.Backend, error) {
			return nil, errors.New("invalid CA or token")
		},
		openStore: func(string) (sandboxcontroller.Store, error) { storeCalls++; return &factoryStore{}, nil },
		newController: func(sandboxcontroller.Store, sandboxcontroller.Backend, sandboxcontroller.Config) (controllerRunner, error) {
			return factoryRunner{}, nil
		},
	}
	if code := runWith(context.Background(), controllerEnvironment(t), factories); code != 2 || storeCalls != 0 {
		t.Fatalf("backend failure opened database: code=%d calls=%d", code, storeCalls)
	}
}

func TestRunWithExecutionFailureClosesStore(t *testing.T) {
	store := &factoryStore{}
	factories := runtimeFactories{
		newBackend: func(sandboxcontroller.KubernetesConfig) (sandboxcontroller.Backend, error) {
			return &factoryBackend{}, nil
		},
		openStore: func(string) (sandboxcontroller.Store, error) { return store, nil },
		newController: func(sandboxcontroller.Store, sandboxcontroller.Backend, sandboxcontroller.Config) (controllerRunner, error) {
			return factoryRunner{err: errors.New("health failed before claim")}, nil
		},
	}
	if code := runWith(context.Background(), controllerEnvironment(t), factories); code != 1 || !store.closed {
		t.Fatalf("execution failure cleanup: code=%d closed=%v", code, store.closed)
	}
}

func controllerEnvironment(t *testing.T) func(string) string {
	t.Helper()
	secret := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(secret, []byte("postgres://controller@database/blazn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE": secret,
		"BLAZN_SANDBOX_CONTROLLER_WORKER_ID":         "controller-1",
		"KUBERNETES_SERVICE_HOST":                    "10.96.0.1",
		"KUBERNETES_SERVICE_PORT_HTTPS":              "443",
		"BLAZN_SANDBOX_IO_IMAGE":                     "registry.example.test/blazn/sandbox-io@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	return func(key string) string { return values[key] }
}
