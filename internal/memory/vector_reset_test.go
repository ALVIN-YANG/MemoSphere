package memory

import (
	"context"
	"errors"
	"testing"
)

type testEncoder struct{ fail bool }

func (v *testEncoder) External() bool { return true }
func (v *testEncoder) ID() string     { return "fixture-new" }
func (v *testEncoder) Search(ctx context.Context, text string) ([]float32, error) {
	return []float32{1, 0}, nil
}
func (v *testEncoder) Documents(ctx context.Context, texts []string) ([][]float32, error) {
	if v.fail {
		return nil, errors.New("fixture unavailable")
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func TestReplaceVectorIndexKeepsMemories(t *testing.T) {
	s, key, p := workerFixture(t, nil)
	ctx := t.Context()
	var id string
	s.DB.transaction(ctx, true, func(q tx) any {
		save(q, "owner", "", Row{"text": "敏感偏好", "sensitive": true, "project_id": p})
		id = str(save(q, "owner", "", Row{"text": "旅行优先乘高铁", "project_id": p})["id"])
		q.exec("INSERT INTO memory_vectors(memory_id,revision,model,vector) VALUES(?,1,'old',?)", id, []byte{1, 2, 3, 4})
		return nil
	})
	encoder := &testEncoder{fail: true}
	s.Vectors = encoder
	if s.PrepareVectorIndex(ctx, false) == nil || s.PrepareVectorIndex(ctx, true) == nil {
		t.Fatal("mismatch and failed probe must stop")
	}
	var model string
	if err := s.DB.QueryRow("SELECT model FROM memory_vectors WHERE memory_id=?", id).Scan(&model); err != nil || model != "old" {
		t.Fatal("failed probe cleared previous index", err)
	}
	encoder.fail = false
	if err := s.PrepareVectorIndex(ctx, true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM memory_vectors").Scan(&count); err != nil || count != 0 {
		t.Fatal("reset should empty vectors", err)
	}
	if err := s.PrepareVectorIndex(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !s.backfill(ctx) {
		t.Fatal("missing backfill")
	}
	result := s.recall(ctx, key, Row{"query": "远行", "purpose": "cite", "project_id": p})
	if str(result["retrieval"].(Row)["model"]) != encoder.ID() {
		t.Fatal("wrong model for retrieval")
	}
	if err := s.DB.QueryRow("SELECT model FROM memory_vectors WHERE memory_id=? AND revision=1", id).Scan(&model); err != nil || model != encoder.ID() {
		t.Fatal("missing replacement vector", err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM memory_vectors").Scan(&count); err != nil || count != 1 {
		t.Fatal("remote backfill included sensitive records", err)
	}
	local := s.recall(ctx, key, Row{"purpose": "cite", "query": "高铁", "local_only": true, "project_id": p})
	if local["retrieval"].(Row)["model"] != nil {
		t.Fatal("local-only query used remote embeddings")
	}
	var text string
	if err := s.DB.QueryRow("SELECT text FROM memories WHERE id=?", id).Scan(&text); err != nil || text != "旅行优先乘高铁" {
		t.Fatal("memory changed", err)
	}
}
