//go:build !darwin && !linux

package plugin

import (
	"errors"
	"os"
)

func newBrokerSocketPair() (*os.File, *os.File, error) {
	return nil, nil, errors.New("plugin broker transport is unsupported on this platform")
}
