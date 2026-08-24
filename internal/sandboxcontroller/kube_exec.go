package sandboxcontroller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
	"golang.org/x/net/websocket"
)

const (
	kubernetesExecProtocol = "v5.channel.k8s.io"
	streamStdin            = byte(0)
	streamStdout           = byte(1)
	streamStderr           = byte(2)
	streamError            = byte(3)
	streamClose            = byte(255)
	maxExecInputBytes      = sandboxio.MaxManifestBytes + sandboxio.MaxHeaderBytes + 4
	maxExecOutputBytes     = sandboxio.MaxArtifactBytes + sandboxio.MaxHeaderBytes + 4
	maxExecStatusBytes     = 64 << 10
)

type kubernetesExecTransport struct {
	baseURL   string
	tlsConfig *tls.Config
	readToken func() (string, error)
	dialer    *net.Dialer
}

type kubernetesPodOwnerVerifier struct {
	baseURL string
	client  *http.Client
}

func newKubernetesExecTransport(config KubernetesConfig) (*kubernetesExecTransport, error) {
	endpoint, err := url.Parse(config.BaseURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		return nil, errors.New("Kubernetes exec endpoint is invalid")
	}
	roots, err := readKubernetesCA(config.CAFile)
	if err != nil {
		return nil, err
	}
	if _, err := readProjectedServiceAccountToken(config.TokenFile); err != nil {
		return nil, err
	}
	return &kubernetesExecTransport{baseURL: strings.TrimSuffix(config.BaseURL, "/"),
		tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: endpoint.Hostname()},
		readToken: func() (string, error) { return readProjectedServiceAccountToken(config.TokenFile) },
		dialer:    &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}}, nil
}

func (t *kubernetesExecTransport) Exec(ctx context.Context, target sandboxio.FrozenPodTarget, command []string, input io.Reader, output io.Writer) error {
	if t == nil || t.tlsConfig == nil || t.readToken == nil || t.dialer == nil || input == nil || output == nil ||
		target.Namespace != sandboxcontrol.Namespace || len(command) != 2 || command[0] != sandboxio.HelperBinary || command[1] != "bootstrap" && command[1] != "release" && command[1] != "artifact" {
		return errors.New("Kubernetes exec request is outside the helper boundary")
	}
	stdin, err := io.ReadAll(io.LimitReader(input, maxExecInputBytes+1))
	if err != nil || len(stdin) > maxExecInputBytes {
		return errors.New("Kubernetes exec input exceeds the helper boundary")
	}
	token, err := t.readToken()
	if err != nil {
		return err
	}
	values := url.Values{"container": []string{target.Container}, "stdin": []string{"true"}, "stdout": []string{"true"}, "stderr": []string{"true"}, "tty": []string{"false"}}
	for _, argument := range command {
		values.Add("command", argument)
	}
	location := strings.Replace(t.baseURL, "https://", "wss://", 1) + "/api/v1/namespaces/" + url.PathEscape(target.Namespace) + "/pods/" + url.PathEscape(target.PodName) + "/exec?" + values.Encode()
	origin := t.baseURL
	configuration, err := websocket.NewConfig(location, origin)
	if err != nil {
		return errors.New("Kubernetes exec WebSocket configuration is invalid")
	}
	configuration.Protocol = []string{kubernetesExecProtocol}
	configuration.TlsConfig = t.tlsConfig.Clone()
	configuration.Dialer = t.dialer
	configuration.Header = http.Header{"Authorization": []string{"Bearer " + token}, "User-Agent": []string{"blazn-sandbox-controller-exec/v1"}}
	connection, err := configuration.DialContext(ctx)
	if err != nil {
		return errors.New("Kubernetes exec WebSocket handshake failed")
	}
	defer connection.Close()
	if connection.Config() == nil || len(connection.Config().Protocol) != 1 || connection.Config().Protocol[0] != kubernetesExecProtocol {
		return errors.New("Kubernetes exec did not negotiate v5.channel.k8s.io")
	}
	deadline := time.Now().Add(kubernetesRequestTimeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	for len(stdin) != 0 {
		count := len(stdin)
		if count > 32<<10 {
			count = 32 << 10
		}
		message := append([]byte{streamStdin}, stdin[:count]...)
		if err := websocket.Message.Send(connection, message); err != nil {
			return errors.New("Kubernetes exec stdin write failed")
		}
		stdin = stdin[count:]
	}
	if err := websocket.Message.Send(connection, []byte{streamClose, streamStdin}); err != nil {
		return errors.New("Kubernetes exec stdin close failed")
	}
	written, stderrBytes := int64(0), int64(0)
	for {
		var message []byte
		if err := websocket.Message.Receive(connection, &message); err != nil {
			return errors.New("Kubernetes exec ended without a status frame")
		}
		if len(message) == 0 {
			return errors.New("Kubernetes exec returned an empty stream frame")
		}
		switch message[0] {
		case streamStdout:
			body := message[1:]
			if int64(len(body)) > maxExecOutputBytes-written {
				return errors.New("Kubernetes exec stdout exceeds the helper boundary")
			}
			if err := writeExecOutput(output, body); err != nil {
				return err
			}
			written += int64(len(body))
		case streamStderr:
			stderrBytes += int64(len(message) - 1)
			if stderrBytes > maxExecStatusBytes {
				return errors.New("Kubernetes exec stderr exceeds the helper boundary")
			}
		case streamError:
			return decodeExecStatus(message[1:], stderrBytes)
		case streamClose:
			if len(message) != 2 || message[1] > streamError {
				return errors.New("Kubernetes exec close frame is invalid")
			}
		default:
			return errors.New("Kubernetes exec returned an unknown stream")
		}
	}
}

func writeExecOutput(output io.Writer, body []byte) error {
	for len(body) != 0 {
		count, err := output.Write(body)
		if err != nil {
			return err
		}
		if count <= 0 || count > len(body) {
			return io.ErrShortWrite
		}
		body = body[count:]
	}
	return nil
}

func decodeExecStatus(body []byte, stderrBytes int64) error {
	if len(body) == 0 || len(body) > maxExecStatusBytes {
		return errors.New("Kubernetes exec status frame is invalid")
	}
	var status struct {
		APIVersion string          `json:"apiVersion"`
		Kind       string          `json:"kind"`
		Metadata   json.RawMessage `json:"metadata"`
		Status     string          `json:"status"`
		Message    string          `json:"message"`
		Reason     string          `json:"reason"`
		Code       int             `json:"code"`
		Details    struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&status); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("Kubernetes exec status frame is invalid")
	}
	if status.Status == "Success" {
		if status.Reason != "" || status.Message != "" || status.Code != 0 && status.Code != http.StatusOK || len(status.Details.Causes) != 0 || stderrBytes != 0 {
			return errors.New("Kubernetes exec success status is inconsistent")
		}
		return nil
	}
	if status.Status != "Failure" {
		return errors.New("Kubernetes exec status is invalid")
	}
	return errors.New("Kubernetes helper process failed")
}

func (v kubernetesPodOwnerVerifier) VerifyPodOwner(ctx context.Context, target sandboxio.FrozenPodTarget) error {
	if v.client == nil || v.baseURL == "" || target.Namespace != sandboxcontrol.Namespace {
		return errors.New("Kubernetes Pod owner verifier is unavailable")
	}
	endpoint := strings.TrimSuffix(v.baseURL, "/") + "/api/v1/namespaces/" + url.PathEscape(target.Namespace) + "/pods/" + url.PathEscape(target.PodName)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "blazn-sandbox-controller-pod-owner/v1")
	response, err := v.client.Do(request)
	if err != nil {
		return errors.New("Kubernetes Pod owner observation failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Kubernetes Pod owner observation returned HTTP %d", response.StatusCode)
	}
	var pod struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name            string                  `json:"name"`
			Namespace       string                  `json:"namespace"`
			UID             string                  `json:"uid"`
			OwnerReferences []networkPolicyOwnerRef `json:"ownerReferences"`
		} `json:"metadata"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&pod); err != nil || decoder.Decode(&struct{}{}) != io.EOF || pod.APIVersion != "v1" || pod.Kind != "Pod" ||
		pod.Metadata.Name != target.PodName || pod.Metadata.Namespace != target.Namespace || pod.Metadata.UID != target.PodUID || len(pod.Metadata.OwnerReferences) != 1 {
		return errors.New("Kubernetes Pod identity changed")
	}
	owner := pod.Metadata.OwnerReferences[0]
	if owner.APIVersion != sandboxcontrol.APIVersion || owner.Kind != sandboxcontrol.Kind || owner.UID != target.SandboxUID || !owner.Controller {
		return errors.New("Kubernetes Pod controller owner changed")
	}
	return nil
}
