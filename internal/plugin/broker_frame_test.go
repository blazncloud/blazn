package plugin

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestBrokerFrameRoundTripAndBounds(t *testing.T) {
	var buffer bytes.Buffer
	input := brokerFrame{Type: brokerFrameRequest, StreamID: 7, Payload: []byte(`{"request":true}`)}
	if err := writeBrokerFrame(&buffer, input); err != nil {
		t.Fatal(err)
	}
	output, err := readBrokerFrame(&buffer)
	if err != nil || output.Type != input.Type || output.StreamID != input.StreamID || !bytes.Equal(output.Payload, input.Payload) {
		t.Fatalf("output=%#v err=%v", output, err)
	}
	if err := writeBrokerFrame(io.Discard, brokerFrame{Type: brokerFrameRequest, StreamID: 1, Payload: make([]byte, brokerMaxControlBytes+1)}); err == nil {
		t.Fatal("oversized control frame passed")
	}
}

func TestBrokerFrameRejectsHeaderDrift(t *testing.T) {
	valid := func() []byte {
		var buffer bytes.Buffer
		if err := writeBrokerFrame(&buffer, brokerFrame{Type: brokerFrameRequest, StreamID: 1}); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	for _, mutate := range []func([]byte){func(v []byte) { v[0] = 'X' }, func(v []byte) { v[4] = 2 }, func(v []byte) { v[5] = 99 }, func(v []byte) { v[6] = 1 }, func(v []byte) { binary.BigEndian.PutUint32(v[8:12], 0) }} {
		value := append([]byte(nil), valid()...)
		mutate(value)
		if _, err := readBrokerFrame(bytes.NewReader(value)); err == nil {
			t.Fatal("invalid broker header passed")
		}
	}
}

func TestBrokerSessionDescribesOnlyAvailableTransport(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil {
		t.Skip(err)
	}
	defer root.Close()
	defer child.Close()
	session := newBrokerSession(root, "content", validRuntimeContext(t))
	done := make(chan error, 1)
	go func() { done <- session.serve() }()
	request := []byte(`{"schemaVersion":1,"requestId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","method":"broker.describe","params":{}}`)
	if err := writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: 9, Payload: request}); err != nil {
		t.Fatal(err)
	}
	frame, err := readBrokerFrame(child)
	if err != nil {
		t.Fatal(err)
	}
	var response brokerResponse
	if json.Unmarshal(frame.Payload, &response) != nil || !response.OK || response.ResultSchema != "broker-description/v1" || !strings.Contains(response.Payload, `"availableCapabilities":["broker.describe"]`) {
		t.Fatalf("response=%s", frame.Payload)
	}
	_ = child.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBrokerSessionRejectsDuplicateStreamsAndSignalsCancellation(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil {
		t.Skip(err)
	}
	session := newBrokerSession(root, "content", validRuntimeContext(t))
	done := make(chan error, 1)
	go func() { done <- session.serve() }()
	request := []byte(`{"schemaVersion":1,"requestId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","method":"broker.describe","params":{}}`)
	_ = writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: 3, Payload: request})
	if _, err := readBrokerFrame(child); err != nil {
		t.Fatal(err)
	}
	_ = writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: 3, Payload: request})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("err=%v", err)
	}
	_ = child.Close()
	_ = root.Close()
	root2, child2, err := newBrokerSocketPair()
	if err != nil {
		t.Fatal(err)
	}
	session2 := newBrokerSession(root2, "content", validRuntimeContext(t))
	session2.cancel()
	frame, err := readBrokerFrame(child2)
	if err != nil || frame.Type != brokerFrameCancel || frame.StreamID != brokerControlStreamID {
		t.Fatalf("frame=%#v err=%v", frame, err)
	}
	_ = root2.Close()
	_ = child2.Close()
}

func TestBrokerSessionBoundsTotalStreams(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil {
		t.Skip(err)
	}
	session := newBrokerSession(root, "content", validRuntimeContext(t))
	for stream := uint32(1); stream <= brokerMaxStreams; stream++ {
		session.seen[stream] = true
	}
	done := make(chan error, 1)
	go func() { done <- session.serve() }()
	request := []byte(`{"schemaVersion":1,"requestId":"cccccccccccccccccccccccccccccccc","method":"broker.describe","params":{}}`)
	if err := writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: brokerMaxStreams + 1, Payload: request}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "stream limit") {
		t.Fatalf("err=%v", err)
	}
	_ = root.Close()
	_ = child.Close()
}
