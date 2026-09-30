package node

import (
	"strings"
	"testing"
)

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

func TestMicroK8sJoinRunsInSnapContextWithTokenOnStdinOnly(t *testing.T) {
	args := microK8sJoinArguments()
	if len(args) != 7 || args[0] != "run" || args[1] != "--shell" || args[2] != "microk8s" || args[3] != "-c" || args[6] != microK8sJoinStdinProgram {
		t.Fatalf("unexpected join invocation %#v", args)
	}
	for _, arg := range args {
		if strings.Contains(arg, ":25000/") {
			t.Fatalf("join URL leaked into argv: %q", arg)
		}
	}
	if !strings.Contains(microK8sJoinStdinProgram, `sys.path.insert(0,"/snap/microk8s/current/scripts/wrappers")`) || !strings.Contains(microK8sJoinStdinProgram, "sys.stdin.readline()") {
		t.Fatal("join program must import MicroK8s wrapper modules and read the URL from stdin")
	}
}
