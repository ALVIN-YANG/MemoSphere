package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Exercise the public MCP schema and authorization on every read and write.
func caseCall(t *testing.T, h http.Handler, key, name string, d Row) Row {
	t.Helper()
	input := Row{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": Row{"name": name, "arguments": d}}
	r := httptest.NewRequest("POST", "/mcp/", strings.NewReader(encoded(input)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var result struct {
		Result struct {
			IsError           bool
			StructuredContent Row
		}
		Error any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Error != nil || result.Result.IsError || result.Result.StructuredContent == nil {
		t.Fatalf("MCP %s: HTTP %d %.800s", name, w.Code, w.Body.String())
	}
	return result.Result.StructuredContent
}

func TestHistoryAcrossClients(t *testing.T) {
	const march = "2026年3月1日我搬到杭州居住"
	const june = "2026年6月1日我改到上海居住"
	const denial = "之前说住在苏州是我记错了，我从没住过苏州"
	var target string
	s, writer, p := workerFixture(t, func(prompt string, data Row) Row {
		msg := str(data["source"].(map[string]any)["text"])
		if prompt == admissionPrompt {
			return Row{"quotes": []string{msg}}
		}
		op := Row{"operation": "ADD", "category": "fact", "quote": msg, "text": msg, "valid_since": "2026年3月1日"}
		if msg == june {
			op["operation"] = "UPDATE"
			op["memory_id"] = target
			op["valid_since"] = "2026年6月1日"
		}
		if msg == denial {
			op["operation"] = "CORRECT"
			op["memory_id"] = target
			op["valid_since"] = ""
		}
		return Row{"operations": []Row{op}}
	})
	var reader string
	s.DB.transaction(t.Context(), true, func(q tx) any {
		a := createAgent(q, "owner", Row{"name": "另一个客户端", "kind": "other", "projects": []string{p}})
		reader = str(s.exchange(q, str(s.setup(q, "owner", str(a["id"]))["code"]))["key"])
		return nil
	})
	h := s.mcpHandler()
	record := func(source, msg string) {
		caseCall(t, h, writer, "retain", Row{"project_id": p, "session_id": source, "messages": []Row{{"source_id": source, "role": "user", "text": msg, "spoken_at": "2026-06-12T12:00:00+08:00"}}})
		if !s.extract(t.Context()) {
			t.Fatal("no job")
		}
	}
	find := func(date, known string) []Row {
		d := Row{"project_id": p, "terms": []string{"居住", "住在", "苏州"}, "purpose": "cite"}
		if date != "" {
			d["valid_at"] = date
		}
		if known != "" {
			d["known_at"] = known
		}
		return objects(caseCall(t, h, reader, "recall", d)["memories"])
	}
	record("one", march)
	if err := s.DB.QueryRow("SELECT id FROM memories WHERE text=?", march).Scan(&target); err != nil {
		t.Fatal(err)
	}
	record("two", june)
	r := find("2026-04-15", "")
	if len(r) != 1 || r[0]["text"] != march || num(r[0]["revision"]) != 1 || len(texts(r[0]["quotes"])) != 0 {
		t.Fatalf("C36 history or cross-agent quotes: %v", r)
	}
	v := r[0]["validity"].(map[string]any)
	if v["valid_from"] != "2026-03-01" || v["valid_to_exclusive"] != "2026-06-01" {
		t.Fatal(v)
	}
	if r = find("", ""); len(r) != 1 || r[0]["text"] != june {
		t.Fatal("C36 current", r)
	}
	// Emulate an earlier receipt clock. A late statement establishes a past
	// effective date but cannot be read using knowledge predating its receipt.
	s.DB.transaction(t.Context(), true, func(q tx) any {
		r := save(q, "owner", "", Row{"text": "我从四月开始居住在青岛", "valid_since": "2026-04-01", "project_id": p})
		ts, _ := time.Parse(time.RFC3339, "2026-06-10T12:34:56+08:00")
		q.exec("UPDATE memory_versions SET recorded_at=? WHERE memory_id=?", float64(ts.Unix()), r["id"])
		return nil
	})
	if r = find("2026-05-01", "2026-05-01"); len(r) != 0 {
		t.Fatal("C37 premature", r)
	}
	if r = find("2026-05-01", "2026-06-10T12:34:56+08:00"); len(r) != 1 || !strings.Contains(str(r[0]["text"]), "青岛") {
		t.Fatal("C37 late", r)
	}
	original := "2026年3月1日我搬到苏州居住"
	record("three", original)
	if err := s.DB.QueryRow("SELECT id FROM memories WHERE text=?", original).Scan(&target); err != nil {
		t.Fatal(err)
	}
	record("four", denial)
	for _, r := range find("2026-04-15", "") {
		if r["id"] == target {
			t.Fatal("C38 resurrected correction", r)
		}
	}
	s.DB.transaction(t.Context(), true, func(q tx) any {
		if detail(q, "owner", target)["versions"].([]Row)[1]["disposition"] != "invalidated" {
			t.Fatal("lost audit")
		}
		erase(q, "owner", target)
		for _, tab := range []string{"memory_versions", "memory_entities", "memory_sources", "memory_vectors", "entity_bindings"} {
			if q.count("SELECT count(*) FROM "+tab+" WHERE memory_id=?", target) != 0 {
				t.Fatal("forgotten data", tab)
			}
		}
		return nil
	})
	for _, r := range find("2026-04-15", "") {
		if r["id"] == target {
			t.Fatal("C38 erased memory", r)
		}
	}
}

func TestFuzzyStartAndHistoricalSwitch(t *testing.T) {
	s, key, p := workerFixture(t, nil)
	s.DB.transaction(t.Context(), true, func(q tx) any {
		save(q, "owner", "", Row{"project_id": p, "text": "我在五月开始兼职", "valid_since": "2026-05"})
		save(q, "owner", "", Row{"project_id": p, "text": "兼职日期不详", "fact_time": "2026-01-01"})
		return nil
	})
	d := Row{"purpose": "cite", "terms": []string{"兼职"}, "project_id": p, "valid_at": "2026-05-10"}
	if r := s.recall(t.Context(), key, d); len(r["memories"].([]Row)) != 0 {
		t.Fatal("guessed start", r)
	}
	d["valid_at"] = "2026-06-01"
	if r := s.recall(t.Context(), key, d); len(r["memories"].([]Row)) != 1 || r["memories"].([]Row)[0]["validity"].(Row)["precision"] != "month" {
		t.Fatal("lost month precision", r)
	}
	t.Setenv("MEMOSPHERE_HISTORICAL_READ", "off")
	if r := s.recall(t.Context(), key, d); len(r["memories"].([]Row)) != 0 || r["retrieval"].(Row)["status"] != "disabled" {
		t.Fatal("history switch returned a current fact", r)
	}
}
