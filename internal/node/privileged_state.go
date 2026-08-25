package node

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/blazncloud/blazn/internal/client"
)

type PrivilegedInstallState struct {
	Client   PrivilegedClient
	Local    StateStore
	Platform string
	mu       sync.RWMutex
	plan     client.NodeInstallPlan
	ctx      context.Context
}

func (s *PrivilegedInstallState) BindContext(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
}

func (s *PrivilegedInstallState) BindPlan(plan client.NodeInstallPlan) {
	s.mu.Lock()
	s.plan = plan
	s.mu.Unlock()
}
func (s *PrivilegedInstallState) request(operation RootOperation) (RootRequest, error) {
	s.mu.RLock()
	plan := s.plan
	s.mu.RUnlock()
	if plan.PlanID == "" {
		return RootRequest{}, errors.New("privileged install state plan is unavailable")
	}
	return RootRequest{SchemaVersion: RootHelperSchema, Operation: operation, Platform: s.Platform, Plan: plan}, nil
}
func (s *PrivilegedInstallState) call(operation RootOperation, wal *InstallWAL, receipt *client.NodeInstallReceipt) (RootResponse, error) {
	request, err := s.request(operation)
	if err != nil {
		return RootResponse{}, err
	}
	request.WAL, request.Receipt = wal, receipt
	s.mu.RLock()
	ctx := s.ctx
	s.mu.RUnlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return s.Client.Call(ctx, request)
}
func (s *PrivilegedInstallState) AcquireInstallLock() (func(), error) {
	return s.Local.AcquireInstallLock()
}
func (s *PrivilegedInstallState) AcquireRuntimeLock() (func(), error) {
	return s.Local.AcquireRuntimeLock()
}
func (s *PrivilegedInstallState) Pin(v EnrollmentPin) error          { return s.Local.Pin(v) }
func (s *PrivilegedInstallState) LoadPin() (EnrollmentPin, error)    { return s.Local.LoadPin() }
func (s *PrivilegedInstallState) SaveRuntime(v RuntimeState) error   { return s.Local.SaveRuntime(v) }
func (s *PrivilegedInstallState) LoadRuntime() (RuntimeState, error) { return s.Local.LoadRuntime() }
func (s *PrivilegedInstallState) CreateWAL(v InstallWAL) error {
	_, err := s.call(RootCreateWAL, &v, nil)
	return err
}
func (s *PrivilegedInstallState) SaveWAL(v InstallWAL) error {
	_, err := s.call(RootSaveWAL, &v, nil)
	return err
}
func (s *PrivilegedInstallState) LoadWAL() (InstallWAL, error) {
	response, err := s.call(RootLoadWAL, nil, nil)
	if err != nil || response.WAL == nil {
		if err == nil && response.ErrorCode == "not_found" {
			err = os.ErrNotExist
		} else if err == nil {
			err = errors.New("root WAL response is invalid")
		}
		return InstallWAL{}, err
	}
	return *response.WAL, nil
}
func (s *PrivilegedInstallState) RemoveWAL() error {
	_, err := s.call(RootRemoveWAL, nil, nil)
	return err
}
func (s *PrivilegedInstallState) SaveReceipt(v client.NodeInstallReceipt) error {
	_, err := s.call(RootSaveReceipt, nil, &v)
	return err
}
func (s *PrivilegedInstallState) LoadReceipt() (client.NodeInstallReceipt, error) {
	response, err := s.call(RootLoadReceipt, nil, nil)
	if err != nil || response.Receipt == nil {
		if err == nil && response.ErrorCode == "not_found" {
			err = os.ErrNotExist
		} else if err == nil {
			err = errors.New("root receipt response is invalid")
		}
		return client.NodeInstallReceipt{}, err
	}
	return *response.Receipt, nil
}
