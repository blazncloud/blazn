//go:build !linux

package process

func defaultRuntimeFactory() RuntimeFactory { return unavailableFactory{} }
