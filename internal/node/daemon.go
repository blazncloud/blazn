package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type CapabilityProvider interface {
	Capability(context.Context) (client.NodeCapability, error)
}

type HeartbeatResult struct {
	NodeID   string `json:"nodeId"`
	BootID   string `json:"bootId"`
	Sequence int64  `json:"sequence"`
	SentAt   string `json:"sentAt"`
}

type Daemon struct {
	api          API
	state        StateStore
	identities   IdentityStore
	capabilities CapabilityProvider
	now          func() time.Time
	mu           sync.Mutex
	bootID       string
	sequence     int64
}

func NewDaemon(api API, state StateStore, identities IdentityStore, capabilities CapabilityProvider) *Daemon {
	return &Daemon{api: api, state: state, identities: identities, capabilities: capabilities, now: time.Now, sequence: -1}
}

func (d *Daemon) Heartbeat(ctx context.Context) (HeartbeatResult, error) {
	if d.api == nil || d.state == nil || d.identities == nil || d.capabilities == nil {
		return HeartbeatResult{}, errors.New("node daemon dependencies are incomplete")
	}
	state, err := d.state.LoadRuntime()
	if err != nil {
		return HeartbeatResult{}, err
	}
	identity, err := d.identities.LoadOrCreate()
	if err != nil {
		return HeartbeatResult{}, err
	}
	capability, err := d.capabilities.Capability(ctx)
	if err != nil {
		return HeartbeatResult{}, err
	}
	digest, err := client.NodeCapabilityDigest(capability)
	if err != nil {
		return HeartbeatResult{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bootID == "" {
		d.bootID, err = randomToken(24)
		if err != nil {
			return HeartbeatResult{}, err
		}
	}
	d.sequence++
	sentAt := d.now().UTC().Format(time.RFC3339Nano)
	heartbeat := client.NodeHeartbeat{NodeID: state.Exchange.Plan.NodeID, IdentityGeneration: state.Exchange.Identity.Generation, BootID: d.bootID, Sequence: d.sequence, SentAt: sentAt, CapabilityDigest: digest, Capability: capability}
	proof, err := nodeProof(identity.PrivateKey, "blazn-node-heartbeat-v1", heartbeat)
	if err != nil {
		return HeartbeatResult{}, err
	}
	if err := d.api.SubmitNodeHeartbeat(ctx, proof, heartbeat); err != nil {
		return HeartbeatResult{}, err
	}
	return HeartbeatResult{NodeID: heartbeat.NodeID, BootID: heartbeat.BootID, Sequence: heartbeat.Sequence, SentAt: sentAt}, nil
}

func nodeProof(privateKey ed25519.PrivateKey, prefix string, body any) (string, error) {
	canonical, err := canonicalJSON(body)
	if err != nil {
		return "", err
	}
	signature := ed25519.Sign(privateKey, append([]byte(prefix+"\n"), canonical...))
	return base64.RawURLEncoding.EncodeToString(signature), nil
}
func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := writeCanonical(&output, normalized); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
func writeCanonical(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		encoded, _ := json.Marshal(typed)
		output.Write(encoded)
	case json.Number:
		if _, err := strconv.ParseFloat(string(typed), 64); err != nil {
			return err
		}
		output.WriteString(string(typed))
	case []any:
		output.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonical(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			output.Write(encoded)
			output.WriteByte(':')
			if err := writeCanonical(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON type %T", value)
	}
	return nil
}
