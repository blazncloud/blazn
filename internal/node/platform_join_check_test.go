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

func TestPreferredJoinURLMatchesSignedAPIServerHost(t *testing.T) {
	check := "0123456789abcdef0123456789abcdef/fee24a55f47e"
	urls := []string{"10.90.0.10:25000/" + check, "100.92.226.60:25000/" + check, "192.168.0.100:25000/" + check, "192.168.0.108:25000/" + check}
	if got := preferredJoinURL(urls, "https://192.168.0.108:16443"); got != "192.168.0.108:25000/"+check {
		t.Fatalf("selected %q", got)
	}
	if got := preferredJoinURL(urls, "https://192.168.0.200:16443"); got != urls[0] {
		t.Fatalf("fallback selected %q", got)
	}
}
