package state

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const SchemaVersion = "proxy/v1alpha1"

var EnvironmentNames = [...]string{
	"OPENAI_BASE_URL",
	"OPENAI_API_KEY",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
}

var (
	ErrNotFound           = errors.New("proxy state not found")
	ErrInvalidState       = errors.New("invalid proxy state")
	ErrOwnershipAmbiguous = errors.New("proxy state ownership is ambiguous")
	ErrLifecycleConflict  = errors.New("proxy lifecycle conflict")
	ErrRecoveryRequired   = errors.New("proxy recovery required")
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type PolicyIdentity struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Digest  string `json:"digest"`
}

type BinaryIdentity struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type ListenerIdentity struct {
	PID                    int    `json:"pid"`
	ProcessStartIdentity   string `json:"processStartIdentity"`
	ExecutableIdentity     string `json:"executableIdentity"`
	Address                string `json:"address"`
	ListenerKeyFingerprint string `json:"listenerKeyFingerprint"`
}

type JournalEnvironment struct {
	Name               string  `json:"name"`
	PriorPresent       bool    `json:"priorPresent"`
	PriorValue         *string `json:"priorValue,omitempty"`
	DesiredValueDigest string  `json:"desiredValueDigest"`
	ActivationMarker   string  `json:"activationMarker"`
	RollbackAction     string  `json:"rollbackAction"`
}

type ReceiptEnvironment struct {
	Name               string `json:"name"`
	DesiredValueDigest string `json:"desiredValueDigest"`
	ActivationMarker   string `json:"activationMarker"`
}

type RollbackAction struct {
	Ordinal   int    `json:"ordinal"`
	Operation string `json:"operation"`
	Target    string `json:"target"`
}

type Journal struct {
	SchemaVersion   string               `json:"schemaVersion"`
	ActivationID    string               `json:"activationId"`
	Nonce           string               `json:"nonce"`
	Generation      int64                `json:"generation"`
	State           string               `json:"state"`
	OwnerUID        int                  `json:"ownerUid"`
	Platform        string               `json:"platform"`
	Mode            string               `json:"mode"`
	SessionIdentity string               `json:"sessionIdentity"`
	Policy          PolicyIdentity       `json:"policy"`
	Binary          BinaryIdentity       `json:"binary"`
	Listener        ListenerIdentity     `json:"listener"`
	Environment     []JournalEnvironment `json:"environment"`
	CA              any                  `json:"ca,omitempty"`
	RollbackActions []RollbackAction     `json:"rollbackActions"`
	CreatedAt       time.Time            `json:"createdAt"`
	UpdatedAt       time.Time            `json:"updatedAt"`
	Checksum        string               `json:"checksum"`
}

type Receipt struct {
	SchemaVersion        string               `json:"schemaVersion"`
	ActivationID         string               `json:"activationId"`
	Nonce                string               `json:"nonce"`
	Generation           int64                `json:"generation"`
	OwnerUID             int                  `json:"ownerUid"`
	JournalDigest        string               `json:"journalDigest"`
	PolicyDigest         string               `json:"policyDigest"`
	Platform             string               `json:"platform"`
	Mode                 string               `json:"mode"`
	SessionIdentity      string               `json:"sessionIdentity"`
	Binary               BinaryIdentity       `json:"binary"`
	Listener             ListenerIdentity     `json:"listener"`
	PublicationMechanism string               `json:"publicationMechanism"`
	Environment          []ReceiptEnvironment `json:"environment"`
	RollbackSummary      []RollbackAction     `json:"rollbackSummary"`
	ActivatedAt          time.Time            `json:"activatedAt"`
	State                string               `json:"state"`
	Checksum             string               `json:"checksum"`
}

func (j *Journal) Validate() error {
	if j.SchemaVersion != SchemaVersion || !uuidPattern.MatchString(j.ActivationID) || !validNonce(j.Nonce) || j.Generation < 1 {
		return fmt.Errorf("%w: invalid journal identity", ErrInvalidState)
	}
	if j.OwnerUID < 0 || !oneOf(j.Platform, "darwin", "linux") || !oneOf(j.Mode, "session", "scoped_run") || j.SessionIdentity == "" || len(j.SessionIdentity) > 256 || j.CA != nil {
		return fmt.Errorf("%w: invalid journal owner or platform", ErrInvalidState)
	}
	if !oneOf(j.State, "prepared", "publishing", "active", "deactivating", "recovery_required") {
		return fmt.Errorf("%w: invalid journal lifecycle state", ErrInvalidState)
	}
	if err := validateDigest(j.Policy.Digest); err != nil || !uuidPattern.MatchString(j.Policy.ID) || j.Policy.Version < 1 {
		return fmt.Errorf("%w: invalid policy identity", ErrInvalidState)
	}
	if err := validateBinaryListener(j.Binary, j.Listener); err != nil {
		return err
	}
	if err := validateJournalEnvironment(j.Environment); err != nil {
		return err
	}
	if j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: missing journal timestamps", ErrInvalidState)
	}
	return validateRollback(j.RollbackActions)
}

func (r *Receipt) Validate() error {
	if r.SchemaVersion != SchemaVersion || !uuidPattern.MatchString(r.ActivationID) || !validNonce(r.Nonce) || r.Generation < 1 || r.OwnerUID < 0 {
		return fmt.Errorf("%w: invalid receipt identity", ErrInvalidState)
	}
	if err := validateDigest(r.JournalDigest); err != nil {
		return fmt.Errorf("%w: invalid journal digest", ErrInvalidState)
	}
	if err := validateDigest(r.PolicyDigest); err != nil {
		return fmt.Errorf("%w: invalid policy digest", ErrInvalidState)
	}
	if !oneOf(r.Platform, "darwin", "linux") || !oneOf(r.Mode, "session", "scoped_run") || r.SessionIdentity == "" || len(r.SessionIdentity) > 256 {
		return fmt.Errorf("%w: invalid receipt platform", ErrInvalidState)
	}
	if !oneOf(r.PublicationMechanism, "process_environment", "launchctl_user_environment", "systemd_user_environment") || !oneOf(r.State, "active", "recovery_required") {
		return fmt.Errorf("%w: invalid receipt state", ErrInvalidState)
	}
	if err := validateBinaryListener(r.Binary, r.Listener); err != nil {
		return err
	}
	if err := validateReceiptEnvironment(r.Environment); err != nil {
		return err
	}
	if r.ActivatedAt.IsZero() {
		return fmt.Errorf("%w: missing activation timestamp", ErrInvalidState)
	}
	return validateRollback(r.RollbackSummary)
}

func validateBinaryListener(binary BinaryIdentity, listener ListenerIdentity) error {
	if !filepath.IsAbs(binary.Path) || filepath.Clean(binary.Path) != binary.Path || validateDigest(binary.Digest) != nil {
		return fmt.Errorf("%w: invalid binary identity", ErrInvalidState)
	}
	if listener.PID < 1 || listener.ProcessStartIdentity == "" || listener.ExecutableIdentity == "" || listener.Address == "" || validateDigest(listener.ListenerKeyFingerprint) != nil {
		return fmt.Errorf("%w: invalid listener identity", ErrInvalidState)
	}
	if err := validateLoopbackAddress(listener.Address); err != nil {
		return err
	}
	return nil
}

func validateLoopbackAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return fmt.Errorf("%w: listener is not a loopback address", ErrInvalidState)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%w: invalid listener port", ErrInvalidState)
	}
	return nil
}

func validateJournalEnvironment(values []JournalEnvironment) error {
	if len(values) != len(EnvironmentNames) {
		return fmt.Errorf("%w: journal must contain exactly five environment records", ErrInvalidState)
	}
	seen := make(map[string]bool, len(values))
	for _, item := range values {
		if !validEnvironmentName(item.Name) || seen[item.Name] || validateDigest(item.DesiredValueDigest) != nil || len(item.ActivationMarker) < 16 {
			return fmt.Errorf("%w: invalid or duplicate environment record", ErrInvalidState)
		}
		seen[item.Name] = true
		if item.PriorPresent {
			if item.PriorValue == nil || item.RollbackAction != "restore_prior_value" {
				return fmt.Errorf("%w: invalid prior value rollback", ErrInvalidState)
			}
		} else if item.PriorValue != nil || item.RollbackAction != "remove_blazn_value" {
			return fmt.Errorf("%w: invalid absent value rollback", ErrInvalidState)
		}
	}
	return nil
}

func validateReceiptEnvironment(values []ReceiptEnvironment) error {
	if len(values) != len(EnvironmentNames) {
		return fmt.Errorf("%w: receipt must contain exactly five environment records", ErrInvalidState)
	}
	seen := make(map[string]bool, len(values))
	for _, item := range values {
		if !validEnvironmentName(item.Name) || seen[item.Name] || validateDigest(item.DesiredValueDigest) != nil || len(item.ActivationMarker) < 16 {
			return fmt.Errorf("%w: invalid or duplicate receipt environment record", ErrInvalidState)
		}
		seen[item.Name] = true
	}
	return nil
}

func validateRollback(actions []RollbackAction) error {
	ordinals := make([]int, 0, len(actions))
	for _, action := range actions {
		if action.Ordinal < 1 || action.Target == "" || !oneOf(action.Operation, "restore_environment", "stop_listener", "remove_scoped_state") {
			return fmt.Errorf("%w: invalid rollback action", ErrInvalidState)
		}
		ordinals = append(ordinals, action.Ordinal)
	}
	sort.Ints(ordinals)
	for i, ordinal := range ordinals {
		if i > 0 && ordinal == ordinals[i-1] {
			return fmt.Errorf("%w: duplicate rollback ordinal", ErrInvalidState)
		}
	}
	return nil
}

func validEnvironmentName(name string) bool {
	for _, candidate := range EnvironmentNames {
		if name == candidate {
			return true
		}
	}
	return false
}

func validateDigest(value string) error {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return ErrInvalidState
	}
	for _, c := range value[7:] {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return ErrInvalidState
		}
	}
	return nil
}

func validNonce(value string) bool {
	if len(value) < 32 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
