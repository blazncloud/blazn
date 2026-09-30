package node

import "testing"

func TestJoinCertificateCheckMatchesMicroK8s(t *testing.T) {
	for _, value := range []string{"fee24a55f47e", "check-value-123456"} {
		if !validJoinCertificateCheck(value) {
			t.Fatalf("%q rejected", value)
		}
	}
	for _, value := range []string{"", "short", "fee24a55f47", "fee24a55 f47e", "fee24a55f47e/x"} {
		if validJoinCertificateCheck(value) {
			t.Fatalf("%q accepted", value)
		}
	}
}
