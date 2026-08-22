//go:build !linux

package microk8sissuer

import "fmt"

type Server struct{}

func (*Server) Serve(string) error { return fmt.Errorf("MicroK8s issuer is Linux-only") }
