package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type compactBatch struct {
	stream   Row
	messages []Row
	refs     map[string]Row
	payload  Row
	lease    string
}

var eligibleStates = []string{"done", "skipped", "cancelled"}

func (s *Service) claimBatch(q tx) *compactBatch {
	streams := q.rows(`SELECT s.*,j.state job_state,j.next_attempt,j.lease_until,coalesce(sum(CASE WHEN m.managed=1 THEN m.raw_bytes ELSE 0 END),0) raw_size FROM conversation_streams s JOIN compaction_jobs j ON j.stream_id=s.id LEFT JOIN messages m ON m.stream_id=s.id GROUP BY s.id ORDER BY s.last_activity`)
	totals := map[string]int64{}
	sessions := map[string]int64{}
	sessionKey := func(r Row) string {
		return encoded([]any{r["user_id"], r["agent_id"], r["session_id"], r["project_id"]})
	}
	for _, r := range streams {
		n := num(r["raw_size"]) + num(r["summary_bytes"])
		totals[str(r["user_id"])] += n
		sessions[sessionKey(r)] += n
	}
	for _, r := range streams {
		if num(r["raw_size"]) == 0 || r["job_state"] == "running" && num(r["lease_until"]) > int64(now()) || num(r["next_attempt"]) > int64(now()) {
			continue
		}
		pressure := sessions[sessionKey(r)] >= int64(s.Policy.StreamTrigger) || totals[str(r["user_id"])] >= int64(s.Policy.UserTrigger)
		if !pressure && !in(str(r["job_state"]), []string{"pending", "running", "failed"}) {
			continue
		}
		rows := q.rows(`SELECT m.*,j.state extraction_state,EXISTS(SELECT 1 FROM memory_sources x JOIN memories f ON f.id=x.memory_id WHERE x.message_id=m.id AND f.sensitive=1) classified_sensitive FROM messages m JOIN extraction_jobs j ON j.message_id=m.id WHERE m.stream_id=? AND m.content_state='raw' AND m.managed=1 ORDER BY m.id`, r["id"])
		end := len(rows)
		bytes := int64(0)
		if now()-r["last_activity"].(float64) < float64(s.Policy.IdleSeconds) {
			for end > 0 {
				n := num(rows[end-1]["raw_bytes"])
				if end < len(rows) && (len(rows)-end >= s.Policy.KeepRecent || n+bytes > int64(s.Policy.RecentBytes)) {
					break
				}
				bytes += n
				end--
			}
		}
		batch := []Row{}
		bytes = 0
		for _, m := range rows[:end] {
			n := num(m["raw_bytes"])
			if !in(str(m["extraction_state"]), eligibleStates) || len(batch) > 0 && (len(batch) >= s.Policy.MaxBatchMessages || n+bytes > int64(s.Policy.MaxBatchBytes)) {
				break
			}
			batch = append(batch, m)
			bytes += n
		}
		if len(batch) == 0 {
			q.exec("UPDATE compaction_jobs SET state='pending',next_attempt=?,error='waiting_for_eligible_prefix' WHERE stream_id=?", now()+5, r["id"])
			continue
		}
		b := &compactBatch{stream: r, messages: batch, lease: token(20), refs: map[string]Row{}, payload: Row{}}
		prior, current := []Row{}, []Row{}
		add := func(ref string, source Row) Row {
			b.refs[ref] = source
			v := pick(source, "role", "recorded_at")
			v["ref"], v["text"] = ref, redact(str(source["text"]))
			return v
		}
		for i, e := range objects(parseJSON(str(r["summary"]))) {
			e["text"] = e["quote"]
			prior = append(prior, add(fmt.Sprintf("s:%d", i), e))
		}
		for _, m := range batch {
			src := clone(m)
			src["message_id"], src["start"], src["sensitive"] = m["id"], 0, truth(m["sensitive"]) || truth(m["classified_sensitive"]) || phonePattern.MatchString(str(m["text"]))
			current = append(current, add(fmt.Sprintf("m:%v", m["id"]), src))
		}
		b.payload["previous"], b.payload["messages"] = prior, current
		q.exec("UPDATE compaction_jobs SET state='running',lease_token=?,lease_until=?,attempts=attempts+1,updated_at=? WHERE stream_id=?", b.lease, now()+2*s.Model.Timeout+30, now(), r["id"])
		return b
	}
	return nil
}
func (b *compactBatch) current(q tx) {
	ensure(q.one(`SELECT 1 FROM conversation_streams s JOIN compaction_jobs j ON j.stream_id=s.id WHERE s.id=? AND s.summary_revision=? AND j.lease_token=? AND j.state='running'`, b.stream["id"], b.stream["summary_revision"], b.lease) != nil, "stale_snapshot")
	rows := q.rows("SELECT m.id,m.content_revision,m.content_hash,j.state FROM messages m JOIN extraction_jobs j ON j.message_id=m.id WHERE m.stream_id=? AND m.id<=? AND m.content_state='raw' AND m.managed=1 ORDER BY m.id", b.stream["id"], b.messages[len(b.messages)-1]["id"])
	ensure(len(rows) == len(b.messages), "stale_snapshot")
	for i, r := range rows {
		m := b.messages[i]
		ensure(r["id"] == m["id"] && r["content_revision"] == m["content_revision"] && r["content_hash"] == m["content_hash"] && in(str(r["state"]), eligibleStates), "stale_snapshot")
	}
}
func (s *Service) compact(ctx context.Context) (worked bool) {
	b := s.DB.transaction(ctx, true, func(q tx) any { return s.claimBatch(q) }).(*compactBatch)
	if b == nil {
		return false
	}
	worked = true
	var generation, verification int64
	defer func() {
		if p := recover(); p != nil {
			code := failure(p)
			s.outcome(generation, code)
			s.outcome(verification, code)
			_, e := s.DB.Exec("UPDATE compaction_jobs SET state='failed',lease_token=NULL,error=?,next_attempt=?+min(300,(1 << min(attempts,8))),updated_at=? WHERE stream_id=? AND state='running' AND lease_token=?", code, now(), now(), b.stream["id"], b.lease)
			must(e)
		}
	}()
	last := b.messages[len(b.messages)-1]["id"]
	var raw Row
	raw, generation = s.call(ctx, last, "compaction", summaryPrompt, b.payload)
	var proposal struct {
		Fragments []struct {
			Ref, Quote string
			Sensitive  bool
		}
	}
	strict(raw, &proposal, "invalid_summary")
	ensure(proposal.Fragments != nil && len(proposal.Fragments) <= 64, "invalid_summary")
	entries := []Row{}
	seen := map[string]bool{}
	for _, f := range proposal.Fragments {
		src := b.refs[f.Ref]
		ensure(src != nil && size(f.Ref) <= 100 && f.Quote != "" && size(f.Quote) <= 1200 && !secret(f.Quote), "unsupported_excerpt")
		text := str(src["text"])
		at := strings.Index(text, f.Quote)
		ensure(at >= 0, "unsupported_excerpt")
		start := num(src["start"]) + int64(size(text[:at]))
		k := encoded([]any{src["message_id"], start, f.Quote})
		if seen[k] {
			continue
		}
		seen[k] = true
		r := pick(src, "role", "message_id", "recorded_at")
		r["quote"], r["start"], r["end"], r["sensitive"] = f.Quote, start, start+int64(size(f.Quote)), truth(src["sensitive"]) || f.Sensitive || phonePattern.MatchString(f.Quote)
		entries = append(entries, r)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, c := entries[i], entries[j]
		if num(a["message_id"]) == num(c["message_id"]) {
			return num(a["start"]) < num(c["start"])
		}
		return num(a["message_id"]) < num(c["message_id"])
	})
	value := encoded(entries)
	n := len(value)
	if len(entries) == 0 {
		n = 0
	}
	old := num(b.stream["summary_bytes"])
	for _, m := range b.messages {
		old += num(m["raw_bytes"])
	}
	ensure(n <= s.Policy.MaxSummaryBytes, "summary_too_large")
	ensure(int64(n) < old, "not_smaller")
	s.outcome(generation, "proposed")
	s.DB.transaction(ctx, false, func(q tx) any { b.current(q); return nil })
	raw, verification = s.call(ctx, last, "compaction_validation", verificationPrompt, Row{"context": b.payload, "candidate": entries})
	var verdict struct {
		OK          *bool
		Missing     []string `json:"missing_critical"`
		Unsupported []string
	}
	strict(raw, &verdict, "invalid_validation")
	ensure(verdict.OK != nil && verdict.Missing != nil && verdict.Unsupported != nil, "invalid_validation")
	ensure(*verdict.OK && len(verdict.Missing)+len(verdict.Unsupported) == 0, "validation_failed")
	s.DB.transaction(ctx, true, func(q tx) any {
		b.current(q)
		for _, m := range b.messages {
			sourceRanges(q, m["id"], str(m["text"]))
			q.exec("UPDATE messages SET text='',raw_bytes=0,content_state='compacted',content_revision=content_revision+1 WHERE id=?", m["id"])
		}
		r := b.stream
		q.exec("UPDATE conversation_streams SET summary=?,summary_bytes=?,summary_revision=summary_revision+1,covered_messages=covered_messages+?,covered_through=?,updated_at=? WHERE id=?", value, n, len(b.messages), last, now(), r["id"])
		a, c := usageBytes(q, Row{"user_id": r["user_id"]})
		x, y := usageBytes(q, r)
		state := "idle"
		if a+c > int64(s.Policy.UserTarget) || x+y > int64(s.Policy.StreamTarget) {
			state = "pending"
		}
		q.exec("UPDATE compaction_jobs SET state=?,lease_token=NULL,error=NULL,next_attempt=0,attempts=0,updated_at=? WHERE stream_id=?", state, now(), r["id"])
		q.event(str(r["user_id"]), "conversation.compacted", str(r["id"]), Row{"messages": len(b.messages), "saved_bytes": old - int64(n)})
		return nil
	})
	s.outcome(generation, "applied")
	s.outcome(verification, "validated")
	return
}
