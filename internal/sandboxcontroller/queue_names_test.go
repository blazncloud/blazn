package sandboxcontroller

import (
	"testing"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

// The controller reads back what the adapter rendered. With
// BLAZN_SANDBOX_LOCAL_QUEUE=blazn-sandboxes the backend record and receipts
// carry the dedicated queue, while the API still records the logical one.
func TestControllerAcceptsBothReviewedBlaznQueues(t *testing.T) {
	for _, name := range []string{sandboxcontrol.QueueName, sandboxcontrol.DedicatedLocalQueue} {
		if !sandboxcontrol.IsBlaznQueue(name) {
			t.Fatalf("%s must be accepted", name)
		}
	}
	for _, name := range []string{"", "m1-light", "frontro-agent", "blazn-poc2"} {
		if sandboxcontrol.IsBlaznQueue(name) {
			t.Fatalf("%q must be refused", name)
		}
	}
}
