//go:build linux

package session

import (
	"errors"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/proxy/activation"
	"github.com/blazncloud/blazn/internal/proxy/process"
)

func TestRecoveredTokenRequiresExactFiveBoundValues(t *testing.T) {
	token := strings.Repeat("A", 43)
	values := map[string]activation.PriorValue{
		"OPENAI_BASE_URL":      {Present: true, Value: "http://127.0.0.1:8123/v1"},
		"OPENAI_API_KEY":       {Present: true, Value: token},
		"ANTHROPIC_BASE_URL":   {Present: true, Value: "http://127.0.0.1:8123"},
		"ANTHROPIC_API_KEY":    {Present: true, Value: token},
		"ANTHROPIC_AUTH_TOKEN": {Present: true, Value: token},
	}
	if got, err := recoveredToken(values, "127.0.0.1:8123"); err != nil || got != token {
		t.Fatalf("token recovery failed: %v", err)
	}
	for _, mutate := range []func(map[string]activation.PriorValue){
		func(v map[string]activation.PriorValue) { delete(v, "ANTHROPIC_AUTH_TOKEN") },
		func(v map[string]activation.PriorValue) {
			v["OPENAI_API_KEY"] = activation.PriorValue{Present: true, Value: strings.Repeat("B", 43)}
		},
		func(v map[string]activation.PriorValue) {
			v["OPENAI_BASE_URL"] = activation.PriorValue{Present: true, Value: "http://127.0.0.1:9999/v1"}
		},
	} {
		copy := make(map[string]activation.PriorValue, len(values))
		for name, value := range values {
			copy[name] = value
		}
		mutate(copy)
		if _, err := recoveredToken(copy, "127.0.0.1:8123"); !errors.Is(err, process.ErrRestartMaterialUnavailable) {
			t.Fatalf("substituted exact-five state accepted: %v", err)
		}
	}
}
