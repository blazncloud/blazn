package sandboxio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

const (
	BootstrapContainer = "sandbox-bootstrap"
	ArtifactContainer  = "sandbox-artifact-io"
	AccessContainer    = "sandbox-access-io"
	HelperBinary       = "/blazn-sandbox-io"
)

var objectIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var podNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)*$`)

type FrozenPodTarget struct {
	Namespace  string
	PodName    string
	PodUID     string
	SandboxUID string
	Container  string
}

// ExecTransport is intentionally narrower than a Kubernetes client. The
// target carries the already-frozen Pod UID and one exact helper container;
// no implementation is installed by this milestone.
type ExecTransport interface {
	Exec(context.Context, FrozenPodTarget, []string, io.Reader, io.Writer) error
}

// PodOwnerVerifier independently re-observes the exact Pod UID and immutable
// Sandbox controller owner before and after each helper exchange.
type PodOwnerVerifier interface {
	VerifyPodOwner(context.Context, FrozenPodTarget) error
}

type ControllerConfig struct {
	Transport ExecTransport
	Owners    PodOwnerVerifier
	Timeout   time.Duration
}

type Controller struct {
	transport ExecTransport
	owners    PodOwnerVerifier
	timeout   time.Duration
}

func NewController(config ControllerConfig) (*Controller, error) {
	if config.Transport == nil || config.Owners == nil {
		return nil, errors.New("sandbox I/O controller dependencies are required")
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout < time.Second || timeout > SourceTimeout {
		return nil, errors.New("sandbox I/O controller timeout is invalid")
	}
	return &Controller{transport: config.Transport, owners: config.Owners, timeout: timeout}, nil
}

func (c *Controller) Bootstrap(ctx context.Context, target FrozenPodTarget, manifest SourceManifest) (SourceMaterializationReceipt, error) {
	if err := validateTarget(target, BootstrapContainer); err != nil {
		return SourceMaterializationReceipt{}, err
	}
	body, err := MarshalSourceManifest(manifest)
	if err != nil {
		return SourceMaterializationReceipt{}, err
	}
	response, err := c.exchange(ctx, target, []string{HelperBinary, "bootstrap"}, OperationBootstrap, body, MaxManifestBytes)
	if err != nil {
		return SourceMaterializationReceipt{}, err
	}
	receipt, err := DecodeSourceMaterializationReceipt(response, &manifest)
	if err != nil {
		return SourceMaterializationReceipt{}, err
	}
	return receipt, nil
}

func (c *Controller) Release(ctx context.Context, target FrozenPodTarget, receiptDigest string) error {
	if err := validateTarget(target, BootstrapContainer); err != nil {
		return err
	}
	body, err := MarshalReleaseRequest(ReleaseRequest{ReceiptDigest: receiptDigest})
	if err != nil {
		return err
	}
	response, err := c.exchange(ctx, target, []string{HelperBinary, "release"}, OperationRelease, body, 1024)
	if err != nil {
		return err
	}
	var receipt ReleaseReceipt
	if err := decodeClosed(response, &receipt); err != nil || receipt.SchemaVersion != SourceReceiptVersion || receipt.ReceiptDigest != receiptDigest || !receipt.Released {
		return protocolError("bootstrap_release_mismatch", err)
	}
	return nil
}

func (c *Controller) ReadArtifact(ctx context.Context, target FrozenPodTarget, artifactPath string) (Artifact, error) {
	if err := validateTarget(target, ArtifactContainer); err != nil {
		return Artifact{}, err
	}
	body, err := MarshalArtifactRequest(ArtifactRequest{Path: artifactPath})
	if err != nil {
		return Artifact{}, err
	}
	response, err := c.exchange(ctx, target, []string{HelperBinary, "artifact"}, OperationArtifact, body, MaxArtifactBytes)
	if err != nil {
		return Artifact{}, err
	}
	digest := SuccessHeader(OperationArtifact, response).SHA256
	return Artifact{Body: response, SHA256: digest, Size: int64(len(response))}, nil
}

func (c *Controller) exchange(ctx context.Context, target FrozenPodTarget, command []string, operation string, body []byte, maxBody int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.owners.VerifyPodOwner(ctx, target); err != nil {
		return nil, fmt.Errorf("sandbox helper preflight owner check failed: %w", err)
	}
	var request bytes.Buffer
	if err := EncodeRequest(&request, operation, body); err != nil {
		return nil, err
	}
	output := newBoundedBuffer(int64(MaxHeaderBytes) + maxBody + 4)
	execErr := c.transport.Exec(ctx, target, append([]string(nil), command...), bytes.NewReader(request.Bytes()), output)
	ownerErr := c.owners.VerifyPodOwner(ctx, target)
	if ownerErr != nil {
		return nil, fmt.Errorf("sandbox helper postflight owner check failed: %w", ownerErr)
	}
	if execErr != nil {
		return nil, fmt.Errorf("sandbox helper transport failed: %w", execErr)
	}
	if output.overflow {
		return nil, protocolError("response_body_too_large", nil)
	}
	reader := bytes.NewReader(output.Bytes())
	_, response, err := DecodeResponse(ctx, reader, operation, maxBody)
	if err != nil {
		return nil, err
	}
	if reader.Len() != 0 {
		return nil, protocolError("protocol_injection", nil)
	}
	return response, nil
}

func validateTarget(target FrozenPodTarget, expectedContainer string) error {
	if !namePattern.MatchString(target.Namespace) || !podNamePattern.MatchString(target.PodName) || len(target.PodName) > 253 ||
		!objectIdentityPattern.MatchString(target.PodUID) || !objectIdentityPattern.MatchString(target.SandboxUID) || target.Container != expectedContainer {
		return errors.New("sandbox helper target is invalid")
	}
	return nil
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	overflow bool
}

func newBoundedBuffer(limit int64) *boundedBuffer { return &boundedBuffer{limit: limit} }
func (b *boundedBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.overflow = true
		return 0, io.ErrShortWrite
	}
	if int64(len(value)) > remaining {
		written, _ := b.buffer.Write(value[:remaining])
		b.overflow = true
		return written, io.ErrShortWrite
	}
	return b.buffer.Write(value)
}
func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }
