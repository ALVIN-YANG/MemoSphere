package memory

import (
	"context"
	"strings"
)

// Screen an explicitly submitted batch together. Fact mutations still run in
// source order so the second statement can correct a fact created by the first.
const batchAdmissionPrompt = "以下 sources 来自同一授权会话。逐条依据原有摘录规则挑选，只从对应正文提取连续原话，确认、指代和更正可结合前文理解。仅输出 JSON 对象 {items:[{ref:0,quotes:[原话]}]}，每个 ref 一项，无可保留内容填空数组。按上述规则选择，批次输出格式以 items 为准。"

func (s *Service) prepareRetainAdmission(ctx context.Context, first any, lease string) {
	var batch []Row
	s.DB.transaction(ctx, true, func(q tx) any {
		op := q.one(`SELECT o.id FROM retain_operations o JOIN retain_items i ON i.operation_id=o.id WHERE i.message_id=? AND o.item_count>1 AND o.batch_checked=0 ORDER BY o.rowid LIMIT 1`, first)
		if op == nil {
			return nil
		}
		q.exec("UPDATE retain_operations SET batch_checked=1 WHERE id=?", op["id"])
		rows := q.rows(`SELECT m.* FROM retain_items i JOIN messages m ON m.id=i.message_id JOIN extraction_jobs j ON j.message_id=m.id
   WHERE i.operation_id=? AND ((j.state='running' AND j.lease_token=?) OR j.state='pending') AND m.role='user' AND m.content_state='raw'
   AND m.sensitive=(SELECT sensitive FROM messages WHERE id=?) ORDER BY m.id LIMIT 64`, op["id"], lease, first)
		total := 0
		for _, m := range rows {
			total += size(str(m["text"]))
			if total > 24000 {
				break
			}
			grantFor(q, m)
			batch = append(batch, m)
		}
		return nil
	})
	if len(batch) < 2 {
		return
	}
	payload := []Row{}
	for i, m := range batch {
		payload = append(payload, Row{"ref": i, "text": redact(str(m["text"])), "role": m["role"]})
	}
	var used int64
	// An unusable prefilter must never discard a message. The normal per-source
	// pipeline is the fallback and will record its own reproducible failure state.
	defer func() {
		if p := recover(); p != nil {
			s.outcome(used, "batch_fallback")
			if ctx.Err() != nil {
				panic(p)
			}
		}
	}()
	raw, id := s.call(ctx, first, "batch_admission", admissionPrompt+"\n"+batchAdmissionPrompt, Row{"sources": payload})
	used = id
	var result struct {
		Items []struct {
			Ref    *int
			Quotes []string
		}
	}
	strict(raw, &result, "invalid_batch_admission")
	ensure(len(result.Items) == len(batch), "invalid_batch_admission")
	stored := map[int][][2]int{}
	for _, item := range result.Items {
		ensure(item.Ref != nil && *item.Ref >= 0 && *item.Ref < len(batch) && item.Quotes != nil && len(item.Quotes) <= 12, "invalid_batch_admission")
		idx := *item.Ref
		_, duplicate := stored[idx]
		ensure(!duplicate, "invalid_batch_admission")
		stored[idx] = [][2]int{}
		for _, quote := range item.Quotes {
			start := strings.Index(str(batch[idx]["text"]), quote)
			ensure(start >= 0 && strings.TrimSpace(quote) != "" && strings.Contains(str(payload[idx]["text"]), quote), "invalid_batch_admission")
			stored[idx] = append(stored[idx], [2]int{start, start + len(quote)})
		}
	}
	s.DB.transaction(ctx, true, func(q tx) any {
		ensure(q.one("SELECT 1 FROM extraction_jobs WHERE message_id=? AND lease_token=? AND state='running'", first, lease) != nil, "stale_snapshot")
		for i, m := range batch {
			grantFor(q, m)
			q.exec(`INSERT OR IGNORE INTO retain_admissions(message_id,spans) SELECT m.id,? FROM messages m JOIN extraction_jobs j ON j.message_id=m.id
    WHERE m.id=? AND m.content_revision=? AND (j.state='pending' OR j.lease_token=?)`, encoded(stored[i]), m["id"], m["content_revision"], lease)
		}
		return nil
	})
	s.outcome(used, "batch_screened")
}
