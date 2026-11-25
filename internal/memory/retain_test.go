package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRetainConcurrentReplayAndCorrection(t *testing.T) {
	var calls atomic.Int32
	ctx := t.Context()
	s, key, p := workerFixture(t, func(prompt string, payload Row) Row {
		calls.Add(1)
		msg := payload["source"].(map[string]any)["text"]
		op := Row{"operation": "ADD", "category": "fact", "text": msg, "quote": msg}
		if msg == "更正为苏州门店" {
			op["operation"] = "CORRECT"
			op["memory_id"] = "M1"
		}
		return Row{"operations": []Row{op}}
	})
	data := func(id, src, body string) Row {
		return Row{"project_id": p, "async": true, "session_id": "chat", "request_id": id, "messages": []Row{{"source_id": src, "role": "user", "text": body}}}
	}
	original := data("request-one", "one", "我订了杭州门店")
	const repeats = 8
	ids := make(chan string, repeats)
	var wg sync.WaitGroup
	for range repeats {
		wg.Add(1)
		go func() { defer wg.Done(); r := s.retain(ctx, key, original); ids <- str(r["operation_id"]) }()
	}
	wg.Wait()
	close(ids)
	var op string
	for id := range ids {
		if op != "" && op != id {
			t.Fatal("split a replay across operations")
		}
		op = id
	}
	if calls.Load() != 0 || s.operation(ctx, key, op)["status"] != "pending" {
		t.Fatal("retain did not enqueue asynchronously")
	}
	if !s.extract(ctx) || s.extract(ctx) || s.operation(ctx, key, op)["status"] != "completed" || calls.Load() != 1 {
		t.Fatal("duplicate extraction")
	}
	replay := s.retain(ctx, key, original)
	if replay["operation_id"] != op || replay["status"] != "completed" || s.extract(ctx) {
		t.Fatal("completed request not idempotent")
	}
	corrected := data("request-two", "two", "更正为苏州门店")
	corrected["messages"] = append(original["messages"].([]Row), corrected["messages"].([]Row)...)
	o2 := str(s.retain(ctx, key, corrected)["operation_id"])
	if !s.extract(ctx) || s.extract(ctx) || s.operation(ctx, key, o2)["status"] != "completed" || calls.Load() != 2 {
		t.Fatal("overlap reprocessed a prior source")
	}
	h := s.mcpHandler()
	result := caseCall(t, h, key, "recall", Row{"project_id": p, "query": "门店", "max_tokens": 1024})
	facts := objects(result["memories"])
	if len(facts) != 1 || num(facts[0]["revision"]) != 2 || facts[0]["text"] != "更正为苏州门店" {
		t.Fatal("old version at completion", facts)
	}
	tiny := caseCall(t, h, key, "recall", Row{"project_id": p, "query": "门店", "max_tokens": 1})
	if len(objects(tiny["memories"])) != 0 || tiny["retrieval"].(map[string]any)["status"] != "budget_exhausted" {
		t.Fatal(tiny)
	}
	// An invalid second source rolls back both the first source and the receipt.
	bad := data("partial", "new", "不应入库")
	bad["messages"] = append(bad["messages"].([]Row), Row{"source_id": "one", "text": "冲突", "role": "user"})
	var err error
	func() { defer catch(&err); s.retain(ctx, key, bad) }()
	if err == nil {
		t.Fatal("accepted conflicting source")
	}
	var n int
	if e := s.DB.QueryRow("SELECT count(*) FROM messages WHERE source_id='new'").Scan(&n); e != nil || n != 0 {
		t.Fatal("partial batch written", n, e)
	}
}

func TestApplicationRetainSurface(t *testing.T) {
	s, _, _ := workerFixture(t, nil)
	// The hosted fixture must never be used as an application DB.
	db, e := Open(t.TempDir() + "/app.sqlite")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	core, e := New(db, "http://localhost", nil)
	if e != nil {
		t.Fatal(e)
	}
	core.Model = s.Model
	secret := strings.Repeat("a", 40)
	h, e := core.ApplicationHandler(secret)
	if e != nil {
		t.Fatal(e)
	}
	request := func(subject, method, path string, d Row) (int, Row) {
		r := httptest.NewRequest(method, "/v1/app/"+path, strings.NewReader(encoded(d)))
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("X-Memory-Subject", subject)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var res Row
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return w.Code, res
	}
	d := Row{"session_id": "s", "request_id": "same", "messages": []Row{{"source_id": "1", "role": "user", "text": "偏好无烟房"}}}
	status, res := request("a", "POST", "retain", d)
	if status != 202 {
		t.Fatal(status, res)
	}
	id := str(res["operation_id"])
	if status, _ = request("b", "GET", "operations/"+id, nil); status != 404 {
		t.Fatal("leaked receipt", status)
	}
	if status, _ = request("a", "GET", "operations/"+id, nil); status != 200 {
		t.Fatal(status)
	}
	d["session_id"] = "different"
	if status, _ = request("a", "POST", "retain", d); status != 409 {
		t.Fatal(status)
	}
	d["async"] = false
	if status, _ = request("a", "POST", "retain", d); status != 400 {
		t.Fatal(status)
	}
	for _, old := range []string{"record_message", "read_conversation", "read_context", "recall_context"} {
		if status, _ = request("a", http.MethodPost, old, d); status != 404 {
			t.Fatal("stale public entry", old, status)
		}
	}
}

func TestRetainScreensAnExplicitBatchOnce(t *testing.T) {
	var screening, extractions atomic.Int32
	s, key, p := workerFixture(t, func(prompt string, data Row) Row {
		if strings.Contains(prompt, batchAdmissionPrompt) {
			screening.Add(1)
			return Row{"items": []Row{{"ref": 0, "quotes": []string{}}, {"ref": 1, "quotes": []string{"我偏好靠窗"}}, {"ref": 2, "quotes": []string{}}}}
		}
		extractions.Add(1)
		return Row{"operations": []Row{{"operation": "ADD", "category": "preference", "text": "我偏好靠窗", "quote": "我偏好靠窗"}}}
	})
	ctx := t.Context()
	d := Row{"project_id": p, "session_id": "test", "messages": []Row{
		{"role": "user", "source_id": "1", "text": "你好"}, {"role": "user", "source_id": "2", "text": "我偏好靠窗"}, {"role": "user", "source_id": "3", "text": "收到"}}}
	op := s.retain(ctx, key, d)
	for s.extract(ctx) {
	}
	replay := s.retain(ctx, key, d)
	if op["operation_id"] != replay["operation_id"] || replay["status"] != "completed" || s.extract(ctx) || screening.Load() != 1 || extractions.Load() != 1 {
		t.Fatal(replay, screening.Load(), extractions.Load())
	}
}
