package sandboxcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

type kubernetesAgentNodeObserver struct {
	baseURL, clusterID string
	client             *http.Client
}
type observedPlacementMetadata struct{ Name, UID, ResourceVersion string }

func (o *kubernetesAgentNodeObserver) AgentNodeObservationEnabled() bool { return true }
func (o *kubernetesAgentNodeObserver) ObserveAgentNode(ctx context.Context, admission sandboxcontrol.AdmissionObservation) (AgentNodeObservation, error) {
	if sandboxcontrol.ValidateAdmissionObservation(admission) != nil {
		return AgentNodeObservation{}, errors.New("Agent Node admission observation is invalid")
	}
	var pod struct {
		Metadata observedPlacementMetadata `json:"metadata"`
		Spec     struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
	}
	podPath := "/api/v1/namespaces/" + url.PathEscape(admission.Pod.Namespace) + "/pods/" + url.PathEscape(admission.Pod.Name)
	if err := o.get(ctx, podPath, &pod); err != nil {
		return AgentNodeObservation{}, err
	}
	if pod.Metadata.Name != admission.Pod.Name || pod.Metadata.UID != admission.Pod.UID || pod.Metadata.ResourceVersion != admission.Pod.ResourceVersion ||
		!kubernetesDNSNamePattern.MatchString(pod.Spec.NodeName) || len(pod.Spec.NodeName) > 253 {
		return AgentNodeObservation{}, errors.New("scheduled Pod identity or Node assignment changed")
	}
	var node struct {
		Metadata observedPlacementMetadata `json:"metadata"`
	}
	if err := o.get(ctx, "/api/v1/nodes/"+url.PathEscape(pod.Spec.NodeName), &node); err != nil {
		return AgentNodeObservation{}, err
	}
	if node.Metadata.Name != pod.Spec.NodeName || !workerPattern.MatchString(node.Metadata.UID) || !workerPattern.MatchString(node.Metadata.ResourceVersion) {
		return AgentNodeObservation{}, errors.New("Kubernetes Node identity is invalid")
	}
	return AgentNodeObservation{AdmissionObservationDigest: admission.Digest, PodUID: admission.Pod.UID, PodResourceVersion: admission.Pod.ResourceVersion,
		KubernetesClusterID: o.clusterID, KubernetesNodeName: node.Metadata.Name, KubernetesNodeUID: node.Metadata.UID}, nil
}
func (o *kubernetesAgentNodeObserver) get(ctx context.Context, path string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(o.baseURL, "/")+path, nil)
	if err != nil {
		return errors.New("Kubernetes placement request cannot be constructed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "blazn-sandbox-controller-placement/v1")
	response, err := o.client.Do(request)
	if err != nil {
		return errors.New("Kubernetes placement observation failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Kubernetes placement observation returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxKubernetesResponseBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxKubernetesResponseBytes {
		return errors.New("Kubernetes placement response is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(out) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("Kubernetes placement response is invalid")
	}
	return nil
}
