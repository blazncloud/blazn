package sandboxcontroller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKubernetesAgentNodeObserverFreezesScheduledPodWithoutReadingTheNode(t *testing.T) {
	admission := storeObservationFixture()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			if r.URL.Path != "/api/v1/namespaces/"+admission.Pod.Namespace+"/pods/"+admission.Pod.Name {
				t.Fatalf("pod path=%s", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"metadata":{"name":"` + admission.Pod.Name + `","uid":"` + admission.Pod.UID + `","resourceVersion":"` + admission.Pod.ResourceVersion + `"},"spec":{"nodeName":"worker-a"}}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	observer := &kubernetesAgentNodeObserver{baseURL: server.URL, clusterID: "cluster-a", client: server.Client()}
	got, err := observer.ObserveAgentNode(context.Background(), admission)
	if err != nil || got.AdmissionObservationDigest != admission.Digest || got.PodUID != admission.Pod.UID || got.PodResourceVersion != admission.Pod.ResourceVersion || got.KubernetesClusterID != "cluster-a" || got.KubernetesNodeName != "worker-a" || requests != 1 {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}

func TestKubernetesAgentNodeObserverRejectsPodIdentityDrift(t *testing.T) {
	admission := storeObservationFixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"metadata":{"name":"` + admission.Pod.Name + `","uid":"replacement","resourceVersion":"` + admission.Pod.ResourceVersion + `"},"spec":{"nodeName":"worker-a"}}`))
	}))
	defer server.Close()
	observer := &kubernetesAgentNodeObserver{baseURL: server.URL, clusterID: "cluster-a", client: server.Client()}
	if _, err := observer.ObserveAgentNode(context.Background(), admission); err == nil {
		t.Fatal("Pod replacement accepted")
	}
}
