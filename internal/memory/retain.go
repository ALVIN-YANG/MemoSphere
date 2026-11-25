package memory

import (
	"context"
	"github.com/google/uuid"
	"net/http"
	"strings"
)

// The receipt is durable before any model work starts. Both overlapping ranges
// and new client request IDs share the same immutable source and worker job.
func (s *Service) retain(ctx context.Context, key string, d Row) Row {
	return s.DB.transaction(ctx, true, func(q tx) any {
		a := checkedAgent(q, key, d, true)
		check(d["async"] == nil || d["async"] == true, "retain 只支持异步提交")
		session := str(d["session_id"])
		check(session != "" && size(session) <= 256, "缺少有效会话")
		items := objects(d["messages"])
		check(len(items) > 0 && len(items) <= 64, "每批需要 1 到 64 条消息")
		messageArgs := make([]Row, 0, len(items))
		seen := map[string]bool{}
		sensitive := false
		for _, item := range items {
			for k := range item {
				check(in(k, []string{"source_id", "text", "role", "spoken_at", "sensitive"}), "消息字段无效")
			}
			check(item["sensitive"] == nil || item["sensitive"] == true || item["sensitive"] == false, "敏感标记无效")
			m := pick(item, "source_id", "role", "spoken_at", "text")
			id := str(m["source_id"])
			check(id != "" && !seen[id], "消息来源缺失或重复")
			seen[id] = true
			m["project_id"], m["session_id"], m["sensitive"] = d["project_id"], session, truth(item["sensitive"])
			sensitive = sensitive || truth(m["sensitive"])
			messageArgs = append(messageArgs, m)
		}
		allowed(!sensitive || truth(a["sensitive_access"]))
		fingerprint := digest(encoded(messageArgs))
		req := str(d["request_id"])
		if d["request_id"] == nil {
			req = fingerprint
		}
		check(strings.TrimSpace(req) != "" && size(req) <= 256, "请求标识无效")
		op := q.one("SELECT * FROM retain_operations WHERE user_id=? AND agent_id=? AND request_key=?", a["user_id"], a["agent_id"], req)
		if op != nil {
			scope(a, op["project_id"])
			allowed(!truth(op["sensitive"]) || truth(a["sensitive_access"]))
			if op["fingerprint"] != fingerprint {
				fail(http.StatusConflict, "请求标识已对应其他输入")
			}
			return retainReceipt(q, op)
		}
		id := uuid.NewString()
		q.exec("INSERT INTO retain_operations(id,user_id,agent_id,project_id,session_id,request_key,fingerprint,item_count,sensitive) VALUES(?,?,?,?,?,?,?,?,?)", id, a["user_id"], a["agent_id"], d["project_id"], session, req, fingerprint, len(items), flag(sensitive))
		for _, m := range messageArgs {
			receipt := s.record(q, key, m)
			if receipt["id"] == nil {
				continue
			} // Owner already forgot this source.
			if !truth(receipt["duplicate"]) && s.Model == nil {
				fail(503, "整理模型尚未配置")
			}
			q.exec("INSERT INTO retain_items(operation_id,message_id) VALUES(?,?)", id, receipt["id"])
		}
		return retainReceipt(q, q.one("SELECT * FROM retain_operations WHERE id=?", id))
	}).(Row)
}

func retainReceipt(q tx, op Row) Row {
	receipt := Row{"operation_id": op["id"], "status": "completed", "item_count": op["item_count"]}
	jobs := q.rows(`SELECT j.state,j.error FROM retain_items i JOIN extraction_jobs j ON j.message_id=i.message_id WHERE i.operation_id=?`, op["id"])
	for _, j := range jobs {
		switch j["state"] {
		case "done":
		case "cancelled":
			if j["error"] != "user_deleted" {
				receipt["status"] = "cancelled"
				return receipt
			}
		case "skipped":
			if j["error"] != "legacy_archive" {
				receipt["status"] = "failed"
				receipt["error"] = j["error"]
				return receipt
			}
		case "failed", "running":
			receipt["status"] = "processing"
		case "pending":
			if receipt["status"] == "completed" {
				receipt["status"] = "pending"
			}
		default:
			fail(500, "未知的整理任务状态")
		}
	}
	return receipt
}

func (s *Service) operation(ctx context.Context, key, id string) Row {
	return s.DB.transaction(ctx, false, func(q tx) any {
		a := agent(q, key)
		op := required(q.one("SELECT * FROM retain_operations WHERE id=? AND user_id=? AND agent_id=?", id, a["user_id"], a["agent_id"]))
		scope(a, op["project_id"])
		allowed(truth(a["write_access"]) && (!truth(op["sensitive"]) || truth(a["sensitive_access"])))
		return retainReceipt(q, op)
	}).(Row)
}
