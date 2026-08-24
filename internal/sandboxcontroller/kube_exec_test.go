package sandboxcontroller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
	"golang.org/x/net/websocket"
)

func TestKubernetesExecUsesOnlyV5HelperChannelsAndPreservesOutput(t *testing.T) {
	var received []byte
	var query url.Values
	websocketServer := websocket.Server{
		Config: websocket.Config{Protocol: []string{kubernetesExecProtocol}},
		Handshake: func(config *websocket.Config, request *http.Request) error {
			if request.Header.Get("Authorization") != "Bearer projected-token" || request.Header.Get("User-Agent") != "blazn-sandbox-controller-exec/v1" {
				return errors.New("exec authentication changed")
			}
			query = request.URL.Query()
			config.Protocol = []string{kubernetesExecProtocol}
			return nil
		},
		Handler: func(connection *websocket.Conn) {
			for {
				var message []byte
				if websocket.Message.Receive(connection, &message) != nil {
					return
				}
				if len(message) == 2 && message[0] == streamClose && message[1] == streamStdin {
					break
				}
				if len(message) == 0 || message[0] != streamStdin {
					return
				}
				received = append(received, message[1:]...)
			}
			_ = websocket.Message.Send(connection, append([]byte{streamStdout}, []byte("response")...))
			status, _ := json.Marshal(map[string]any{"status": "Success", "metadata": map[string]any{}})
			_ = websocket.Message.Send(connection, append([]byte{streamError}, status...))
		},
	}
	server := httptest.NewTLSServer(websocketServer)
	defer server.Close()
	transport := &kubernetesExecTransport{baseURL: server.URL,
		tlsConfig: server.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
		readToken: func() (string, error) { return "projected-token", nil },
		dialer:    &net.Dialer{Timeout: time.Second}}
	target := sandboxio.FrozenPodTarget{Namespace: sandboxcontrol.Namespace, PodName: "pod.sandbox-1", PodUID: "pod-uid", SandboxUID: "sandbox-uid", Container: sandboxio.BootstrapContainer}
	var output bytes.Buffer
	if err := transport.Exec(context.Background(), target, []string{sandboxio.HelperBinary, "bootstrap"}, bytes.NewBufferString("request"), &output); err != nil {
		t.Fatal(err)
	}
	if string(received) != "request" || output.String() != "response" || query.Get("container") != sandboxio.BootstrapContainer ||
		!reflect.DeepEqual(query["command"], []string{sandboxio.HelperBinary, "bootstrap"}) || query.Get("stdin") != "true" || query.Get("tty") != "false" {
		t.Fatalf("received=%q output=%q query=%v", received, output.String(), query)
	}
}

func TestKubernetesExecRejectsProcessFailureAndGenericCommands(t *testing.T) {
	transport := &kubernetesExecTransport{tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12}, readToken: func() (string, error) { return "token", nil }, dialer: &net.Dialer{}}
	target := sandboxio.FrozenPodTarget{Namespace: sandboxcontrol.Namespace, PodName: "pod", PodUID: "pod-uid", SandboxUID: "sandbox-uid", Container: sandboxio.BootstrapContainer}
	if err := transport.Exec(context.Background(), target, []string{"/bin/sh", "-c"}, bytes.NewReader(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("generic exec command was accepted")
	}
	failed, _ := json.Marshal(map[string]any{"status": "Failure", "reason": "NonZeroExitCode", "details": map[string]any{"causes": []map[string]string{{"reason": "ExitCode", "message": "1"}}}})
	if err := decodeExecStatus(failed, 0); err == nil {
		t.Fatal("failed helper process status was accepted")
	}
}

func TestKubernetesPodOwnerVerifierPinsPodAndSandboxUID(t *testing.T) {
	podUID := "pod-uid"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{
			"name": "pod.sandbox-1", "namespace": sandboxcontrol.Namespace, "uid": podUID,
			"ownerReferences": []map[string]any{{"apiVersion": sandboxcontrol.APIVersion, "kind": sandboxcontrol.Kind, "name": "sandbox-1", "uid": "sandbox-uid", "controller": true}},
		}})
	}))
	defer server.Close()
	verifier := kubernetesPodOwnerVerifier{baseURL: server.URL, client: server.Client()}
	target := sandboxio.FrozenPodTarget{Namespace: sandboxcontrol.Namespace, PodName: "pod.sandbox-1", PodUID: "pod-uid", SandboxUID: "sandbox-uid", Container: sandboxio.BootstrapContainer}
	if err := verifier.VerifyPodOwner(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	podUID = "replacement-uid"
	if err := verifier.VerifyPodOwner(context.Background(), target); err == nil {
		t.Fatal("replacement Pod UID was accepted")
	}
}
