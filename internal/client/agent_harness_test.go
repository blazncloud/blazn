package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const ahWorkspace = "00000000-0000-4000-8000-000000000001"
const ahAgent = "00000000-0000-4000-8000-000000000002"
const ahVersion = "00000000-0000-4000-8000-000000000003"

func TestAgentHarnessClientRoutesAndBodies(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			if r.Method != "GET" || r.URL.Path != "/v1/workspaces/"+ahWorkspace+"/agents" || r.URL.Query().Get("cursor") != "next" {
				t.Fatalf("list=%s %s", r.Method, r.URL.String())
			}
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		case 2:
			if r.Method != "POST" || r.URL.Path != "/v1/workspaces/"+ahWorkspace+"/agents/"+ahAgent+"/versions" || r.Header.Get("Idempotency-Key") != "publish-1" {
				t.Fatalf("publish=%s %s", r.Method, r.URL.String())
			}
			var body PublishAgentVersionRequest
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil || body.Version["id"] != ahVersion {
				t.Fatalf("body=%#v err=%v", body, e)
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"agent":{"id":"` + ahAgent + `"},"version":{"id":"` + ahVersion + `"}}`))
		case 3:
			if r.Method != "POST" || r.URL.Path != "/v1/workspaces/"+ahWorkspace+"/harness/profiles/"+ahAgent+"/revisions" {
				t.Fatalf("revise=%s %s", r.Method, r.URL.String())
			}
			_, _ = w.Write([]byte(`{"profile":{"id":"` + ahAgent + `"}}`))
		default:
			t.Fatalf("unexpected request")
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, server.Client())
	if _, e := c.ListAgents(context.Background(), "token", ahWorkspace, "next"); e != nil {
		t.Fatal(e)
	}
	if _, e := c.PublishAgentVersion(context.Background(), "token", ahWorkspace, ahAgent, "publish-1", PublishAgentVersionRequest{Version: JSONDocument{"id": ahVersion}}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.ReviseHarnessProfile(context.Background(), "token", ahWorkspace, ahAgent, "revise-1", ReviseHarnessProfileRequest{Profile: JSONDocument{"id": ahAgent}, ExpectedResourceVersion: 1}); e != nil {
		t.Fatal(e)
	}
}
func TestAgentHarnessClientRejectsInvalidIdentifiers(t *testing.T) {
	c, _ := New("https://example.test", http.DefaultClient)
	if _, e := c.GetAgent(context.Background(), "t", "bad", ahAgent); e == nil {
		t.Fatal("invalid workspace accepted")
	}
	if _, e := c.GetHarnessVersion(context.Background(), "t", ahWorkspace, ahAgent, "bad"); e == nil {
		t.Fatal("invalid version accepted")
	}
	if _, e := c.GetAgent(context.Background(), "t", ahWorkspace, "00000000-0000-4000-8000-00000000000A"); e == nil {
		t.Fatal("uppercase UUID accepted")
	}
	if _, e := c.ListAgents(context.Background(), "t", ahWorkspace, string(make([]byte, 129))); e == nil {
		t.Fatal("oversize cursor accepted")
	}
}
