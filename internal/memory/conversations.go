package memory

import (
	"sort"
	"strings"
)

type Policy struct {
	StreamHard       int64 `json:"stream_hard"`
	StreamTrigger    int64 `json:"stream_trigger"`
	StreamTarget     int64 `json:"stream_target"`
	UserHard         int64 `json:"user_hard"`
	UserTrigger      int64 `json:"user_trigger"`
	UserTarget       int64 `json:"user_target"`
	KeepRecent       int   `json:"keep_recent"`
	RecentBytes      int   `json:"recent_bytes"`
	MaxBatchBytes    int   `json:"max_batch_bytes"`
	MaxBatchMessages int   `json:"max_batch_messages"`
	MaxSummaryBytes  int   `json:"max_summary_bytes"`
	IdleSeconds      int   `json:"idle_seconds"`
}

func defaultPolicy() Policy {
	return Policy{1 << 20, 768 << 10, 512 << 10, 8 << 20, 6 << 20, 4 << 20, 12, 128 << 10, 64 << 10, 80, 64 << 10, 300}
}
func (p Policy) valid() bool {
	return p.StreamTarget > 0 && p.StreamTarget < p.StreamTrigger && p.StreamTrigger < p.StreamHard && p.UserTarget > 0 && p.UserTarget < p.UserTrigger && p.UserTrigger < p.UserHard && min(p.KeepRecent, p.RecentBytes, p.MaxBatchBytes, p.MaxBatchMessages, p.MaxSummaryBytes) > 0 && p.IdleSeconds >= 0
}
func bodyHash(s string, p, role, sensitive any) string {
	return digest(encoded([]any{s, p, role, truth(sensitive)}))
}
func attach(q tx, m Row, managed bool) string {
	sid := digest(encoded([]any{m["user_id"], m["agent_id"], m["session_id"], m["project_id"], m["sensitive"]}))
	q.exec("INSERT OR IGNORE INTO conversation_streams(id,user_id,agent_id,session_id,project_id,sensitive,last_activity,updated_at) VALUES(?,?,?,?,?,?,?,?)", sid, m["user_id"], m["agent_id"], m["session_id"], m["project_id"], m["sensitive"], m["recorded_at"], now())
	q.exec("UPDATE conversation_streams SET last_activity=max(last_activity,?),updated_at=? WHERE id=?", m["recorded_at"], now(), sid)
	q.exec("UPDATE messages SET stream_id=?,content_hash=?,raw_bytes=?,managed=? WHERE id=?", sid, bodyHash(str(m["text"]), m["project_id"], m["role"], m["sensitive"]), len(str(m["text"])), flag(managed), m["id"])
	q.exec("INSERT OR IGNORE INTO compaction_jobs(stream_id,updated_at) VALUES(?,?)", sid, now())
	return sid
}
func usageBytes(q tx, m Row) (raw, summary int64) {
	where := "user_id=?"
	args := []any{m["user_id"]}
	if m["agent_id"] != nil {
		where += " AND agent_id=? AND session_id=? AND project_id IS ?"
		args = append(args, m["agent_id"], m["session_id"], m["project_id"])
	}
	return q.count("SELECT coalesce(sum(raw_bytes),0) FROM messages WHERE managed=1 AND "+where, args...), q.count("SELECT coalesce(sum(summary_bytes),0) FROM conversation_streams WHERE "+where, args...)
}
func (s *Service) storage(q tx, u string) Row {
	raw, summary := usageBytes(q, Row{"user_id": u})
	r := q.one("SELECT count(*) messages,coalesce(sum(content_state='compacted'),0) compacted_messages,coalesce(sum(CASE WHEN managed=0 THEN raw_bytes ELSE 0 END),0) legacy_bytes FROM messages WHERE user_id=?", u)
	r["raw_bytes"], r["summary_bytes"], r["used_bytes"] = raw, summary, raw+summary
	r["limit_bytes"], r["trigger_bytes"], r["stream_limit_bytes"] = s.Policy.UserHard, s.Policy.UserTrigger, s.Policy.StreamHard
	states := Row{}
	for _, x := range q.rows("SELECT j.state,count(*) n FROM compaction_jobs j JOIN conversation_streams s ON s.id=j.stream_id WHERE s.user_id=? GROUP BY j.state", u) {
		states[str(x["state"])] = x["n"]
	}
	r["jobs"] = states
	return r
}
func (s *Service) conversations(q tx, u string) Row {
	items := q.rows(`SELECT m.id,m.agent_id,m.session_id,m.project_id,m.role,m.text,m.recorded_at,c.name agent_name,j.state processing_state,j.error processing_error,j.result processing_result FROM messages m LEFT JOIN connections c ON c.id=m.agent_id LEFT JOIN extraction_jobs j ON j.message_id=m.id WHERE m.user_id=? AND m.content_state='raw' ORDER BY m.id DESC LIMIT 100`, u)
	summaries := q.rows("SELECT s.*,c.name agent_name FROM conversation_streams s LEFT JOIN connections c ON c.id=s.agent_id WHERE s.user_id=? AND s.covered_messages>0 ORDER BY s.updated_at DESC LIMIT 10", u)
	for _, r := range summaries {
		r["entries"] = parseJSON(str(r["summary"]))

	}
	return Row{"items": items, "summaries": summaries, "storage": s.storage(q, u)}
}
func sourceRanges(q tx, id any, text string) {
	for _, src := range q.rows("SELECT memory_id,revision,quote FROM memory_sources WHERE message_id=?", id) {
		quote := str(src["quote"])
		start, end := offsets(text, quote)
		q.exec("UPDATE memory_sources SET source_start=?,source_end=? WHERE memory_id=? AND revision=? AND message_id=? AND quote=?", start, end, src["memory_id"], src["revision"], id, quote)
	}
}
func offsets(text, quote string) (any, any) {
	a := strings.Index(text, quote)
	if a < 0 {
		return nil, nil
	}
	b := strings.LastIndex(text, quote)
	return size(text[:a]), size(text[:b]) + size(quote)
}
func forgetSource(q tx, m, src Row) {
	quote := str(src["quote"])
	text := strings.ReplaceAll(str(m["text"]), quote, "[用户已删除相关内容]")
	q.exec("UPDATE messages SET text=?,content_hash=NULL,raw_bytes=?,content_revision=content_revision+1 WHERE id=?", text, len(text), m["id"])
	if m["content_state"] == "raw" {
		sourceRanges(q, m["id"], text)
	}
	stream := q.one("SELECT * FROM conversation_streams WHERE id=?", m["stream_id"])
	if stream == nil {
		return
	}
	out := []Row{}
	for _, e := range objects(parseJSON(str(stream["summary"]))) {
		if num(e["message_id"]) == num(m["id"]) && (strings.Contains(quote, str(e["quote"])) || strings.Contains(str(e["quote"]), quote) || src["source_start"] != nil && num(e["start"]) < num(src["source_end"]) && num(e["end"]) > num(src["source_start"])) {
			continue
		}
		out = append(out, e)
	}
	bytes := len(encoded(out))
	if len(out) == 0 {
		bytes = 0
	}
	q.exec("UPDATE conversation_streams SET summary=?,summary_bytes=?,summary_revision=summary_revision+1,updated_at=? WHERE id=?", encoded(out), bytes, now(), stream["id"])
	q.exec("UPDATE compaction_jobs SET state='idle',lease_token=NULL,error='source_deleted',updated_at=? WHERE stream_id=?", now(), stream["id"])
}
func checkedAgent(q tx, key string, d Row, write bool) Row {
	a := agent(q, key)
	if d["connection_id"] != nil {
		allowed(a["agent_id"] == d["connection_id"])
	}
	scope(a, d["project_id"])
	if write {
		allowed(truth(a["write_access"]))
	} else {
		allowed(truth(a["read_access"]))
	}
	return a
}
func (s *Service) record(q tx, key string, d Row) Row {
	a := checkedAgent(q, key, d, true)
	text := str(d["text"])
	role := str(d["role"])
	session := str(d["session_id"])
	source := str(d["source_id"])
	sensitive := truth(d["sensitive"])
	check(in(role, []string{"user", "assistant", "tool"}) && strings.TrimSpace(text) != "" && len(text) <= 262144, "消息内容或角色无效")
	check(session != "" && source != "" && size(session) <= 256 && size(source) <= 256, "缺少有效来源标识")
	allowed(!sensitive || truth(a["sensitive_access"]))
	if d["spoken_at"] != nil {
		_, valid := spokenTime(str(d["spoken_at"]))
		check(valid, "原话时间需要带时区的 ISO 8601 时间")
	}
	keys := []any{a["user_id"], a["agent_id"], session, source}
	cond := "user_id=? AND agent_id=? AND session_id=? AND source_id=?"
	if q.one("SELECT 1 FROM deleted_sources WHERE "+cond, keys...) != nil {
		q.exec("UPDATE connections SET last_ingested=? WHERE id=?", now(), a["agent_id"])
		return Row{"id": nil, "duplicate": true, "deleted": true}
	}
	if old := q.one("SELECT * FROM messages WHERE "+cond, keys...); old != nil {
		same := old["text"] == text && old["role"] == role && old["project_id"] == d["project_id"] && truth(old["sensitive"]) == sensitive
		if old["content_state"] == "compacted" {
			same = old["content_hash"] == bodyHash(text, d["project_id"], role, sensitive)
		}
		if !same || old["spoken_at"] != d["spoken_at"] {
			fail(409, "同一来源标识不能对应不同内容")
		}
		q.exec("UPDATE connections SET last_ingested=? WHERE id=?", now(), a["agent_id"])
		return Row{"id": old["id"], "duplicate": true}
	}
	id, e := q.exec("INSERT INTO messages(user_id,agent_id,session_id,source_id,project_id,role,text,sensitive,recorded_at,spoken_at) VALUES(?,?,?,?,?,?,?,?,?,?)", a["user_id"], a["agent_id"], session, source, d["project_id"], role, text, flag(sensitive), now(), d["spoken_at"]).LastInsertId()
	must(e)
	q.exec("INSERT INTO extraction_jobs(message_id,updated_at) VALUES(?,?)", id, now())
	m := q.one("SELECT * FROM messages WHERE id=?", id)
	attach(q, m, true)
	r, z := usageBytes(q, m)
	ur, uz := usageBytes(q, Row{"user_id": a["user_id"]})
	check(r+z <= s.Policy.StreamHard && ur+uz <= s.Policy.UserHard, "对话空间暂满，本条消息未接收，请保留并稍后重试")
	q.exec("UPDATE connections SET last_ingested=? WHERE id=?", now(), a["agent_id"])
	return Row{"id": id, "duplicate": false}
}
func visible(q tx, stream Row, priv bool) []Row {
	filtered := []Row{}
	for _, e := range objects(parseJSON(str(stream["summary"]))) {
		if priv || !truth(e["sensitive"]) && q.count("SELECT count(*) FROM memory_sources s JOIN memories f ON f.id=s.memory_id WHERE s.message_id=? AND f.sensitive=1", e["message_id"]) == 0 {
			filtered = append(filtered, e)
		}
	}
	return filtered
}
func (s *Service) readConversation(q tx, key string, d Row) Row {
	a := checkedAgent(q, key, d, false)
	limit := max(1, min(int(num(val(d, "limit", 12))), 30))
	args := []any{a["user_id"], a["agent_id"], d["session_id"], d["project_id"], a["sensitive_access"]}
	cond := "user_id=? AND agent_id=? AND session_id=? AND project_id IS ? AND (sensitive=0 OR ?)"
	streams := q.rows("SELECT * FROM conversation_streams WHERE "+cond, args...)
	entries := []Row{}
	for _, stream := range streams {
		entries = append(entries, visible(q, stream, truth(a["sensitive_access"]))...)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if num(entries[i]["message_id"]) == num(entries[j]["message_id"]) {
			return num(entries[i]["start"]) < num(entries[j]["start"])
		}
		return num(entries[i]["message_id"]) < num(entries[j]["message_id"])
	})
	msgs := []Row{}
	length := 0
	var cursor any
	for _, m := range q.rows("SELECT id,role,text,recorded_at,sensitive,EXISTS(SELECT 1 FROM memory_sources x JOIN memories f ON f.id=x.memory_id WHERE x.message_id=messages.id AND f.sensitive=1) classified_sensitive FROM messages WHERE "+cond+" AND content_state='raw' AND (? IS NULL OR id<?) ORDER BY id DESC LIMIT ?", append(args, d["before_id"], d["before_id"], limit)...) {
		text := redact(str(m["text"]))
		if len(msgs) > 0 && length+len(text) > s.Policy.RecentBytes {
			break
		}
		cursor = m["id"]
		if !truth(a["sensitive_access"]) && (truth(m["classified_sensitive"]) || phonePattern.MatchString(str(m["text"]))) {
			continue
		}
		m["text"] = text
		msgs = append(msgs, m)
		length += len(text)
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	more := cursor != nil && q.one("SELECT 1 FROM messages WHERE "+cond+" AND content_state='raw' AND id<? LIMIT 1", append(args, cursor)...) != nil
	if !more {
		cursor = nil
	}
	return Row{"kind": "conversation_evidence", "confirmed_facts": false, "summary": entries, "messages": msgs, "has_more_raw": more, "next_before_id": cursor}
}
