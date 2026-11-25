package memory

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"time"
)

// PrepareVectorIndex runs before starting workers. Reset is an explicit one-shot
// operation with the service stopped; the synthetic probe never includes owner data.
func (s *Service) PrepareVectorIndex(ctx context.Context, reset bool) error {
	if s.Vectors == nil {
		if reset {
			return fmt.Errorf("清空向量前需先配置可用的新模型")
		}
		return nil
	}
	if reset {
		if _, err := s.Vectors.Documents(ctx, []string{"索引连通性检查"}); err != nil {
			return err
		}
		_, err := s.DB.ExecContext(ctx, "DELETE FROM memory_vectors")
		return err
	}
	var mismatch bool
	err := s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_vectors WHERE model<>?)", s.Vectors.ID()).Scan(&mismatch)
	if err == nil && mismatch {
		return fmt.Errorf("库中存在其他模型的向量；请停服执行 --reset-vector-index 后再启动")
	}
	return err
}

func (s *Service) backfill(ctx context.Context) bool {
	if s.Vectors == nil {
		return false
	}
	rows := s.DB.transaction(ctx, false, func(q tx) any {
		return q.rows(`SELECT m.id,m.revision,m.text FROM memories m LEFT JOIN memory_vectors v ON v.memory_id=m.id AND v.revision=m.revision AND v.model=? WHERE m.state='active' AND m.basis IN ('user_statement','tool_observation') AND (m.sensitive=0 OR ?) AND v.memory_id IS NULL ORDER BY m.updated_at DESC,m.id LIMIT 12`, s.Vectors.ID(), !s.Vectors.External())
	}).([]Row)
	if len(rows) == 0 {
		return false
	}
	t := []string{}
	for _, r := range rows {
		t = append(t, str(r["text"]))
	}
	vectors, e := s.Vectors.Documents(ctx, t)
	if e != nil {
		if ctx.Err() == nil {
			log.Printf("向量回填失败，稍后重试: %v", e)
			select {
			case <-time.After(30 * time.Second):
			case <-ctx.Done():
			}
		}
		return false
	}
	s.DB.transaction(ctx, true, func(q tx) any {
		for i, r := range rows {
			buf := make([]byte, len(vectors[i])*4)
			for j, v := range vectors[i] {
				binary.LittleEndian.PutUint32(buf[4*j:], math.Float32bits(v))
			}
			q.exec("INSERT OR REPLACE INTO memory_vectors SELECT id,revision,?,? FROM memories WHERE id=? AND revision=? AND state='active' AND basis IN ('user_statement','tool_observation') AND (sensitive=0 OR ?)", s.Vectors.ID(), buf, r["id"], r["revision"], !s.Vectors.External())
		}
		return nil
	})
	return true
}
