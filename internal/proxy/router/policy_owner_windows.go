//go:build windows

package router

import "os"

// Windows activation is deferred. The policy loader still requires a regular
// file; native ACL ownership qualification belongs to that later adapter.
func verifyPolicyOwner(os.FileInfo) error          { return nil }
func openPolicyFile(path string) (*os.File, error) { return os.Open(path) }
