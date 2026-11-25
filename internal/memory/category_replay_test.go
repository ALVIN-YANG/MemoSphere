package memory

import (
	"encoding/json"
	"memosphere/internal/embedding"
	"os"
	"path/filepath"
	"testing"
)

// The answer runner calls this opt-in replay to exercise the same SQLite and BGE path as retain/recall.
func TestCategoryReplay(t *testing.T) {
	output := os.Getenv("MEMOSPHERE_CATEGORY_PACKETS")
	if output == "" {
		t.Skip("run via scripts/evaluate_categories.py")
	}
	var data struct {
		Memories []struct {
			ID, Text string
			Category string `json:"category_id"`
		}
		Questions []struct{ ID, Question, Purpose string }
	}
	raw, err := os.ReadFile("../../scripts/fixtures/category_questions_zh.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(t.TempDir(), "isolated.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, "http://localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := embedding.Open(os.Getenv("MEMOSPHERE_EMBEDDING_DIR"), os.Getenv("MEMOSPHERE_ONNX_LIBRARY"))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	s.Vectors = engine
	ctx := t.Context()
	if err = db.PrepareApplicationDB(ctx); err != nil {
		t.Fatal(err)
	}
	var space Row
	db.transaction(ctx, true, func(q tx) any {
		space = appSpace(q, "synthetic", "")
		for _, m := range data.Memories {
			// Stable fixture IDs make BM25 ties reproducible across runs.
			d := save(q, str(space["owner"]), "", Row{"text": m.Text, "category_id": m.Category})
			q.exec("UPDATE memories SET id=? WHERE id=?", m.ID, d["id"])
			q.exec("UPDATE memory_versions SET memory_id=?,content=json_set(content,'$.id',?) WHERE memory_id=?", m.ID, m.ID, d["id"])
		}
		return nil
	})
	for s.backfill(ctx) {
	}
	packets := map[string]map[string][]Row{}
	for _, question := range data.Questions {
		packets[question.ID] = map[string][]Row{}
		for _, mode := range []string{"untyped", "weighted", "hard"} {
			s.categoryComparison = mode
			if mode == "weighted" {
				s.categoryComparison = ""
			}
			got := s.recallForClient(ctx, str(space["agent_key"]), Row{"project_id": space["project"], "purpose": question.Purpose, "query": question.Question, "max_tokens": 4096})
			meta := got["retrieval"].(Row)
			if truth(meta["budget_limited"]) || meta["model"] != engine.ID() || len(meta["degraded"].([]string)) > 0 {
				t.Fatalf("incomplete retrieval: %v", meta)
			}
			packets[question.ID][mode] = got["memories"].([]Row)
		}
	}
	result, err := json.Marshal(Row{"model": engine.ID(), "packets": packets})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(output, result, 0600); err != nil {
		t.Fatal(err)
	}
}
