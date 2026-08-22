package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

// Watch reconnects with the last emitted event ID, rejects reordered streams,
// and returns only after a terminal ready, failed, or deleted state.
func (s *Service) Watch(ctx context.Context, id, cursor string, emit func(client.SandboxEvent) error) (WatchTerminal, error) {
	lastID, lastSequence := cursor, int64(-1)
	consecutiveErrors := 0
	for {
		stream, err := withAccessToken(ctx, s, func(token string) (EventStream, error) { return s.api.StreamSandboxEvents(ctx, token, id, lastID) })
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			consecutiveErrors++
			if consecutiveErrors >= s.maxErrors {
				return "", &UnavailableError{Cause: err}
			}
			if err := waitContext(ctx, s.reconnect); err != nil {
				return "", err
			}
			continue
		}
		for {
			event, nextErr := stream.Next()
			if nextErr != nil {
				_ = stream.Close()
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				consecutiveErrors++
				if consecutiveErrors >= s.maxErrors {
					if errors.Is(nextErr, io.EOF) {
						nextErr = errors.New("sandbox event stream ended before a terminal event")
					}
					return "", &UnavailableError{Cause: nextErr}
				}
				if err := waitContext(ctx, s.reconnect); err != nil {
					return "", err
				}
				break
			}
			if err := validateEvent(event, id); err != nil {
				_ = stream.Close()
				return "", err
			}
			if event.EventID == lastID {
				continue
			}
			if lastSequence >= 0 && event.Sequence <= lastSequence {
				_ = stream.Close()
				return "", fmt.Errorf("sandbox event sequence did not increase")
			}
			if err := emit(event); err != nil {
				_ = stream.Close()
				return "", err
			}
			consecutiveErrors = 0
			lastID, lastSequence = event.EventID, event.Sequence
			if terminal := terminalEvent(event); terminal != "" {
				_ = stream.Close()
				return terminal, nil
			}
		}
	}
}

func terminalEvent(event client.SandboxEvent) WatchTerminal {
	state, _ := event.Payload["state"].(string)
	typeName := strings.ToLower(event.Type)
	for _, terminal := range []WatchTerminal{WatchReady, WatchFailed, WatchDeleted} {
		value := string(terminal)
		if state == value || typeName == value || strings.HasSuffix(typeName, "."+value) || strings.HasSuffix(typeName, "_"+value) || strings.HasSuffix(typeName, "-"+value) {
			return terminal
		}
	}
	return ""
}
func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
