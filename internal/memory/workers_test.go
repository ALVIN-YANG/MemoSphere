package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func workerFixture(t *testing.T, reply func(string, Row) Row) (*Service, string, string) {
	t.Helper()
	db, e := Open(filepath.Join(t.TempDir(), "fixture.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Messages []struct{ Content string } }
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Messages) != 2 {
			t.Error("invalid request")
			w.WriteHeader(400)
			return
		}
		var payload Row
		if json.Unmarshal([]byte(request.Messages[1].Content), &payload) != nil {
			t.Error("invalid payload")
			w.WriteHeader(400)
			return
		}
		answer := reply(request.Messages[0].Content, payload)
		_ = json.NewEncoder(w).Encode(Row{"choices": []Row{{"finish_reason": "stop", "message": Row{"content": encoded(answer)}}}})
	}))
	t.Cleanup(endpoint.Close)
	s, e := New(db, "http://localhost", nil)
	if e != nil {
		t.Fatal(e)
	}
	s.Model = &Model{Name: "fixture", BaseURL: endpoint.URL, Timeout: 2, client: endpoint.Client()}
	var key, p string
	db.transaction(t.Context(), true, func(q tx) any {
		q.exec("INSERT INTO users VALUES('owner','fixture@example.test','unused',?)", now())
		p = str(project(q, "owner", "隔离项目")["id"])
		a := createAgent(q, "owner", Row{"name": "测试", "kind": "other", "projects": []string{p}})
		code := str(s.setup(q, "owner", str(a["id"]))["code"])
		key = str(s.exchange(q, code)["key"])
		return nil
	})
	return s, key, p
}
func ingest(ctx context.Context, s *Service, key, p, source, text string) Row {
	return s.DB.transaction(ctx, true, func(q tx) any {
		return s.record(q, key, Row{"project_id": p, "session_id": "test", "source_id": source, "role": "user", "text": text})
	}).(Row)
}
func TestCorrectionRechecksOnlyDependentVersions(t *testing.T) {
	const original = "我和父母去湖畔酒店"
	const correction = "其实我独自去了湖畔酒店"
	var parent string
	s, key, p := workerFixture(t, func(prompt string, payload Row) Row {
		if prompt == reviewPrompt {
			return Row{"supported": true, "text": "这次可能只需要单人房"}
		}
		message := payload["source"].(map[string]any)["text"]
		op := Row{"operation": "ADD", "category": "fact", "text": message, "quote": message, "entities": []string{"湖畔酒店"}}
		if message == correction {
			op["operation"], op["memory_id"] = "CORRECT", parent
		}
		return Row{"operations": []Row{op}}
	})
	ingest(t.Context(), s, key, p, "one", original)
	if !s.extract(t.Context()) {
		t.Fatal("missing extraction")
	}
	var child, independent string
	s.DB.transaction(t.Context(), true, func(q tx) any {
		parent = str(required(q.one("SELECT id FROM memories WHERE text=?", original))["id"])
		child = str(save(q, "owner", "", Row{"text": "可能需要家庭房", "project_id": p, "inputs": []Row{{"id": parent, "revision": 1}}})["id"])
		independent = str(save(q, "owner", "", Row{"text": "预订本身仍然有效", "project_id": p})["id"])
		return nil
	})
	ingest(t.Context(), s, key, p, "two", correction)
	s.extract(t.Context())
	s.DB.transaction(t.Context(), false, func(q tx) any {
		r := detail(q, "owner", parent)
		if num(r["revision"]) != 2 || r["versions"].([]Row)[1]["disposition"] != "invalidated" || detail(q, "owner", child)["state"] != "suspended" {
			t.Fatal("correction did not invalidate the dependency")
		}
		return nil
	})
	if !s.review(t.Context()) {
		t.Fatal("missing review")
	}
	s.DB.transaction(t.Context(), true, func(q tx) any {
		d := detail(q, "owner", child)
		refs := d["inputs"].([]Row)
		if d["state"] != "uncertain" || d["text"] != "这次可能只需要单人房" || len(refs) != 1 || num(refs[0]["revision"]) != 2 {
			t.Fatal("review not tied to corrected revision")
		}
		erase(q, "owner", parent)
		if q.one("SELECT id FROM memories WHERE id=?", child) != nil || detail(q, "owner", independent)["state"] != "active" {
			t.Fatal("erasure scope is incorrect")
		}
		return nil
	})
	if !truth(ingest(t.Context(), s, key, p, "two", correction)["deleted"]) {
		t.Fatal("replay resurrected erased source")
	}
}
func TestCompactionRequiresVerifiedPrefix(t *testing.T) {
	const excerpt = "需要无烟房"
	raw := excerpt + "。" + strings.Repeat("普通对话", 90)
	verified := false
	s, key, p := workerFixture(t, func(prompt string, payload Row) Row {
		if prompt == summaryPrompt {
			return Row{"fragments": []Row{{"ref": "m:1", "quote": excerpt}}}
		}
		if prompt != verificationPrompt {
			t.Error("unexpected prompt")
		}
		return Row{"ok": verified, "missing_critical": []string{}, "unsupported": []string{}}
	})
	s.Policy.StreamHard = 10000
	s.Policy.StreamTrigger = 1000
	s.Policy.StreamTarget = 500
	s.Policy.IdleSeconds = 0
	ingest(t.Context(), s, key, p, "one", raw)
	s.DB.transaction(t.Context(), true, func(q tx) any { q.exec("UPDATE extraction_jobs SET state='done'"); return nil })
	if !s.compact(t.Context()) {
		t.Fatal("missing compaction job")
	}
	s.DB.transaction(t.Context(), true, func(q tx) any {
		j := q.one("SELECT state,error FROM compaction_jobs")
		if j["error"] != "validation_failed" || q.one("SELECT text FROM messages")["text"] != raw {
			t.Fatal("unverified summary replaced source")
		}
		q.exec("UPDATE compaction_jobs SET next_attempt=0")
		return nil
	})
	verified = true
	s.compact(t.Context())
	s.DB.transaction(t.Context(), false, func(q tx) any {
		r := s.readConversation(q, key, Row{"project_id": p, "session_id": "test"})
		entries := r["summary"].([]Row)
		if len(entries) != 1 || entries[0]["quote"] != excerpt || len(r["messages"].([]Row)) != 0 || q.count("SELECT count(*) FROM messages WHERE content_state='compacted' AND text=''") != 1 {
			t.Fatal("verified summary did not replace eligible prefix")
		}
		if q.count("SELECT count(*) FROM model_usage WHERE outcome IN ('applied','validated')") != 2 {
			t.Fatal("model accounting missing")
		}
		return nil
	})
	if !truth(ingest(t.Context(), s, key, p, "one", raw)["duplicate"]) {
		t.Fatal("compacted source lost replay identity")
	}
}
