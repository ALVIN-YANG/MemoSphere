package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPBehindHTTPSProxy(t *testing.T) {
	service, key, _ := workerFixture(t, func(string, Row) Row { return nil })
	service.Origin = "https://memory.example.test"
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "memory.example.test"
	req.Header.Set("Origin", service.Origin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configured public host behind loopback proxy: HTTP %d", resp.StatusCode)
	}
	for _, bad := range []struct {
		host, origin string
		status       int
	}{
		{"unconfigured.example.test", service.Origin, http.StatusBadRequest},
		{req.Host, "https://unconfigured.example.test", http.StatusForbidden},
	} {
		other := req.Clone(t.Context())
		other.Body, err = req.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		other.Host = bad.host
		other.Header.Set("Origin", bad.origin)
		denied, err := server.Client().Do(other)
		if err != nil {
			t.Fatal(err)
		}
		denied.Body.Close()
		if denied.StatusCode != bad.status {
			t.Fatalf("unexpected status for host %q, origin %q: %d", bad.host, bad.origin, denied.StatusCode)
		}
	}
	var result struct {
		Result struct{ Tools []struct{ Name string } }
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range result.Result.Tools {
		names[tool.Name] = true
	}
	if len(names) != 2 || !names["retain"] || !names["recall"] {
		t.Fatalf("unexpected tools: %v", names)
	}
}
