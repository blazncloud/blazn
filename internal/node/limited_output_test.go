package node

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestLimitedOutputDrainsVerboseChildWithoutKillingIt(t *testing.T) {
	var head, tail bytes.Buffer
	stdout := &limitedOutput{writer: &head, remaining: 16}
	stderr := &limitedOutput{writer: &tail, remaining: 64, keepTail: true}
	command := exec.Command("/bin/sh", "-c", `i=0; while [ $i -lt 2000 ]; do echo "progress line $i" >&2; i=$((i+1)); done; echo "node root helper failed: final detail" >&2; echo 0123456789abcdefXYZ`)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		t.Fatalf("a child writing past the retained limit must not be killed: %v", err)
	}
	if !stdout.overflow || head.String() != "0123456789abcdef" {
		t.Fatalf("stdout overflow=%v head=%q", stdout.overflow, head.String())
	}
	if !stderr.overflow || tail.Len() > 64 || !strings.HasSuffix(tail.String(), "node root helper failed: final detail\n") {
		t.Fatalf("stderr overflow=%v tail=%q", stderr.overflow, tail.String())
	}
	if detail := rootHelperFailureDetail(tail.String()); detail != "final detail" {
		t.Fatalf("detail=%q", detail)
	}
}
