package memory

import (
	"github.com/google/uuid"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var keyPattern = regexp.MustCompile(`(?i)\b(?:(?:sk-|ms_)[A-Za-z0-9_-]{12,}|Bearer\s+[A-Za-z0-9._-]{16,})`)
var credentialsPattern = regexp.MustCompile(`(?i)((?:api[_ -]?key|access[_ -]?token|password|密码|密钥)["']?\s*(?:[:：=]|是|为)\s*["']?)([^\s，。；;"'，}]+)`)
var pemPattern = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
var phonePattern = regexp.MustCompile(`(?:^|[^0-9])1[3-9][0-9]{9}(?:[^0-9]|$)`)

func redact(s string) string {
	return credentialsPattern.ReplaceAllString(keyPattern.ReplaceAllString(pemPattern.ReplaceAllString(s, "[已屏蔽凭据]"), "[已屏蔽凭据]"), "${1}[已屏蔽凭据]")
}
func secret(s string) bool       { return strings.Contains(s, "[已屏蔽凭据]") || s != redact(s) }
func normalized(s string) string { return cases.Fold().String(strings.TrimSpace(norm.NFKC.String(s))) }
func decodeMemory(r Row) Row {
	d := clone(r)
	d["category_id"] = categoryOf(str(r["category"])).ID
	d["tags"] = parseJSON(str(d["tags"]))
	d["sensitive"] = truth(d["sensitive"])
	return d
}
func unique(a []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range a {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}
func objects(v any) []Row {
	out := []Row{}
	switch x := v.(type) {
	case nil:
	case []Row:
		return x
	case []any:
		for _, i := range x {
			r, ok := i.(map[string]any)
			check(ok, "对象格式无效")
			out = append(out, Row(r))
		}
	default:
		fail(400, "需要对象列表")
	}
	return out
}
func library(q tx, user string, v url.Values) Row {
	where := []string{"user_id=?"}
	args := []any{user}
	state := v.Get("state")
	if state == "" {
		state = "active"
	}
	check(in(state, []string{"all", "uncertain", "active", "suspended"}), "记忆状态无效")
	if state != "all" {
		where = append(where, "state=?")
		args = append(args, state)
	}
	if query := truncated(v.Get("q"), 500); query != "" {
		where = append(where, "(instr(lower(text),lower(?))>0 OR instr(lower(tags),lower(?))>0)")
		args = append(args, query, query)
	}
	if p := v.Get("project"); p != "" {
		if p == "global" {
			where = append(where, "project_id IS NULL")
		} else {
			where = append(where, "project_id=?")
			args = append(args, p)
		}
	}
	if c := v.Get("category"); c != "" {
		where = append(where, "category=?")
		if canonical := categoryOf(c); canonical.ID != "" {
			c = canonical.Label
		}
		args = append(args, c)
	}
	if v.Get("sensitive") == "1" {
		where = append(where, "sensitive=1")
	}
	if e := v.Get("entity"); e != "" {
		where = append(where, "EXISTS(SELECT 1 FROM current_entity_links e WHERE e.entity_id=? AND e.memory_id=memories.id AND e.revision=memories.revision)")
		args = append(args, e)
	}
	order := v.Get("order")
	check(order == "" || order == "created" || order == "updated", "排序参数无效")
	col := "updated_at"
	if order == "created" {
		col = "created_at"
	}
	offset := 0
	if v.Has("offset") {
		n, e := strconv.Atoi(v.Get("offset"))
		check(e == nil, "分页参数无效")
		offset = max(n, 0)
	}
	sql := " FROM memories WHERE " + strings.Join(where, " AND ")
	total := q.count("SELECT count(*)"+sql, args...)
	rows := q.rows("SELECT *"+sql+" ORDER BY "+col+" DESC,id DESC LIMIT 50 OFFSET ?", append(args, offset)...)
	for i, r := range rows {
		rows[i] = decodeMemory(r)
	}
	result := Row{"category_catalog": categoryCatalog, "items": rows, "total": total, "offset": offset}
	if order == "created" {
		categories := []string{}
		for _, c := range categoryCatalog {
			categories = append(categories, c.Label)
		}
		categoryCounts := map[string]int64{}
		stateCounts := map[string]int64{}
		var all int64
		for _, group := range q.rows("SELECT category,state,count(*) AS n FROM memories WHERE user_id=? GROUP BY category,state ORDER BY category,state", user) {
			category := str(group["category"])
			if category == pendingCategory && !in(category, categories) {
				categories = append(categories, category)
			}
			n := num(group["n"])
			categoryCounts[category] += n
			stateCounts[str(group["state"])] += n
			all += n
		}
		result["categories"] = categories
		result["counts"] = Row{"all": all, "states": stateCounts, "categories": categoryCounts}
	}
	return result
}
func entities(q tx, id, rev any) []Row {
	r := q.rows("SELECT entity_id id,name,kind,aliases,qualifier FROM current_entity_links WHERE memory_id=? AND revision=? ORDER BY entity_id", id, rev)
	for _, e := range r {
		e["aliases"] = parseJSON(str(e["aliases"]))
	}
	return r
}
func inputs(q tx, id, rev any) []Row {
	return q.rows("SELECT parent id,parent_revision revision FROM memory_dependencies WHERE child=? AND revision=? ORDER BY parent", id, rev)
}
func evidenceFor(q tx, user string, refs []Row) []Row {
	out := []Row{}
	for _, ref := range refs {
		r := q.one("SELECT v.content FROM memory_versions v JOIN memories m ON m.id=v.memory_id WHERE m.user_id=? AND v.memory_id=? AND v.revision=?", user, ref["id"], ref["revision"])
		if r != nil {
			x := clone(ref)
			x["text"] = parseJSON(str(r["content"])).(map[string]any)["text"]
			x["sources"] = q.rows("SELECT m.role,m.spoken_at,s.quote FROM memory_sources s JOIN messages m ON m.id=s.message_id WHERE m.user_id=? AND s.memory_id=? AND s.revision=? ORDER BY s.message_id,s.quote", user, ref["id"], ref["revision"])
			out = append(out, x)
		}
	}
	return out
}
func detail(q tx, user, id string) Row {
	r := decodeMemory(required(q.one("SELECT * FROM memories WHERE user_id=? AND id=?", user, id)))
	versions := q.rows("SELECT * FROM memory_versions WHERE memory_id=? ORDER BY revision DESC", id)
	for _, v := range versions {
		v["content"] = parseJSON(str(v["content"]))
		v["entities"] = entities(q, id, v["revision"])
		refs := inputs(q, id, v["revision"])
		v["inputs"] = refs
		v["input_evidence"] = evidenceFor(q, user, refs)
		if v["revision"] == r["revision"] {
			r["inputs"], r["input_evidence"] = v["inputs"], v["input_evidence"]
		}
	}
	r["versions"] = versions
	r["entities"] = entities(q, id, r["revision"])
	r["sources"] = q.rows("SELECT s.revision,s.message_id,s.quote,m.agent_id,m.session_id,m.role,m.recorded_at,m.spoken_at FROM memory_sources s JOIN messages m ON m.id=s.message_id WHERE s.memory_id=? ORDER BY s.revision DESC", id)
	return r
}
func writeVersion(q tx, r, old Row, reason, op string, refs []Row) {
	check(len(refs) <= 12, "推断依据过多")
	seen := map[string]bool{}
	for _, ref := range refs {
		// A label-only edit inherits the exact inputs already owned by the prior revision.
		if op == "RECLASSIFY" {
			continue
		}
		p := q.one("SELECT * FROM memories WHERE id=? AND user_id=?", ref["id"], r["user_id"])
		allowed(p != nil && p["project_id"] == r["project_id"])
		id := str(p["id"])
		check(!seen[id] && id != r["id"] && sameRevision(ref["revision"], p["revision"]) && p["state"] != "suspended" && p["basis"] != "unverified", "依据已变更或无效")
		seen[id] = true
		if truth(p["sensitive"]) {
			r["sensitive"] = 1
		}
	}
	q.exec("DELETE FROM memory_reviews WHERE child=?", r["id"])
	if old != nil {
		children := q.rows(`WITH RECURSIVE affected(id,rev) AS (VALUES(?,?) UNION SELECT m.id,m.revision FROM affected a JOIN memory_dependencies d ON d.parent=a.id AND d.parent_revision=a.rev JOIN memories m ON m.id=d.child AND m.revision=d.revision) SELECT id,rev FROM affected WHERE id<>?`, old["id"], old["revision"], old["id"])
		for _, child := range children {
			q.exec("UPDATE memories SET state='suspended' WHERE id=?", child["id"])
			q.exec("INSERT OR REPLACE INTO memory_reviews(child,revision) VALUES(?,?)", child["id"], child["rev"])
		}
		state := map[string]string{"UPDATE": "historical", "RECLASSIFY": "historical", "CORRECT": "invalidated", "UNCERTAIN": "uncertain"}[op]
		if state == "" {
			state = "superseded"
		}
		if old["state"] == "uncertain" {
			state = "uncertain"
		}
		q.exec("UPDATE memory_versions SET disposition=? WHERE memory_id=? AND revision=?", state, r["id"], old["revision"])
		if op == "CORRECT" && old["last_operation"] == "RECLASSIFY" {
			// All intervening label-only versions rest on the same retracted assertion.
			q.exec(`WITH RECURSIVE same_claim(rev) AS (VALUES(?) UNION ALL
 SELECT v.revision-1 FROM same_claim c JOIN memory_versions v ON v.memory_id=? AND v.revision=c.rev
 WHERE v.operation='RECLASSIFY' AND v.revision>1) UPDATE memory_versions SET disposition='invalidated'
 WHERE memory_id=? AND revision IN (SELECT rev FROM same_claim)`, old["revision"], r["id"], r["id"])
		}
	}
	fields := []string{"project_id", "text", "category", "tags", "sensitive", "fact_time", "revision", "updated_at", "state", "origin", "kind", "last_operation", "basis"}
	vals := []any{r["id"], r["user_id"], r["created_at"]}
	updates := []string{}
	for _, f := range fields {
		vals = append(vals, r[f])
		updates = append(updates, f+"=excluded."+f)
	}
	q.exec("INSERT INTO memories(id,user_id,created_at,"+strings.Join(fields, ",")+") VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")+") ON CONFLICT(id) DO UPDATE SET "+strings.Join(updates, ","), vals...)
	for _, ref := range refs {
		q.exec("INSERT INTO memory_dependencies VALUES(?,?,?,?)", r["id"], r["revision"], ref["id"], ref["revision"])
	}
	q.exec("INSERT INTO memory_versions(memory_id,revision,content,reason,recorded_at,disposition,operation) VALUES(?,?,?,?,?,'current',?)", r["id"], r["revision"], encoded(decodeMemory(r)), reason, r["updated_at"], op)
	q.bump(r["user_id"])
}
func save(q tx, user, id string, d Row) Row {
	var old Row
	if id != "" {
		old = required(q.one("SELECT * FROM memories WHERE user_id=? AND id=?", user, id))
		check(sameRevision(d["revision"], old["revision"]), "这条记忆刚被修改，请刷新后再编辑")
		if d["text"] == nil && (d["category_id"] != nil || d["category"] != nil) {
			return reclassify(q, old, writeCategory(d, old))
		}
	}
	text := strings.TrimSpace(str(d["text"]))
	check(size(text) > 0 && size(text) <= 12000 && !secret(text), "记忆内容无效或含有凭据")
	p := nullable(str(d["project_id"]))
	if p != nil {
		checkProjects(q, user, []string{str(p)})
	}
	tags := texts(d["tags"])
	check(len(tags) <= 20, "标签太多")
	for _, t := range tags {
		check(size(t) <= 40, "标签过长")
	}
	n := now()
	r := Row{"user_id": user, "project_id": p, "text": text, "category": writeCategory(d, old), "tags": encoded(unique(tags)), "sensitive": flag(truth(d["sensitive"]) || phonePattern.MatchString(text)), "fact_time": str(d["fact_time"]), "revision": 1, "created_at": n, "updated_at": n, "state": "active", "origin": "manual", "kind": "profile", "last_operation": "ADD", "basis": "user_statement"}
	reason := "用户新增"
	op := "USER_EDIT"
	refs := objects(d["inputs"])
	items := objects(d["entities"])
	if id == "" {
		id = uuid.NewString()
		if p != nil {
			r["kind"] = "project"
		}
		if len(refs) > 0 {
			r["basis"] = "inference"
			r["state"] = "uncertain"
			r["origin"] = "derived"
			r["last_operation"] = "INFER"
			op = "INFER"
		}
	} else {

		if old["category"] != r["category"] && text == old["text"] && p == old["project_id"] && r["fact_time"] == old["fact_time"] &&
			num(old["sensitive"]) == num(r["sensitive"]) && r["tags"] == old["tags"] && len(refs) == 0 && d["entities"] == nil && d["valid_since"] == nil {
			return reclassify(q, old, str(r["category"]))
		}
		check(len(refs) == 0 && (old["basis"] != "inference" || d["confirm_statement"] == true), "请明确确认本人的陈述")
		reason = truncated(str(val(d, "reason", "用户编辑")), 240)
		r["kind"], r["created_at"], r["revision"], r["last_operation"] = old["kind"], old["created_at"], num(old["revision"])+1, op
		if d["entities"] == nil && p == old["project_id"] {
			for _, e := range entities(q, id, old["revision"]) {
				for _, name := range append([]string{str(e["name"])}, texts(e["aliases"])...) {
					if strings.Contains(text, name) {
						items = append(items, e)
						break
					}
				}
			}
		}
	}
	r["id"] = id
	check(r["category"] != "规则" || r["basis"] != "inference", "推断不能作为规则")
	for k, limit := range map[string]int{"category": 40, "fact_time": 120} {
		a := []rune(str(r[k]))
		if len(a) > limit {
			r[k] = string(a[:limit])
		}
	}
	if expr := str(d["valid_since"]); expr != "" {
		check(setValidStart(r, expr, ""), "适用起点需要明确的日期或月份")
	}
	writeVersion(q, r, old, reason, op, refs)
	bindEntities(q, r, items, "", true, true)
	action := "memory.created"
	if old != nil {
		action = "memory.updated"
	}
	q.event(user, action, id, Row{"revision": r["revision"]})
	return detail(q, user, id)
}
func erase(q tx, user, id string) Row {
	required(q.one("SELECT id FROM memories WHERE user_id=? AND id=?", user, id))
	todo := []string{id}
	seen := map[string]bool{}
	for len(todo) > 0 {
		curr := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[curr] {
			continue
		}
		seen[curr] = true
		for _, d := range q.rows(`WITH RECURSIVE t(child,rev) AS (SELECT child,revision FROM memory_dependencies WHERE parent=? UNION SELECT d.child,d.revision FROM t JOIN memory_dependencies d ON d.parent=t.child AND d.parent_revision=t.rev) SELECT t.*,m.revision head FROM t JOIN memories m ON m.id=t.child WHERE m.user_id=?`, curr, user) {
			if d["rev"] == d["head"] {
				todo = append(todo, str(d["child"]))
			} else {
				for _, t := range []string{"memory_versions", "memory_sources"} {
					q.exec("DELETE FROM "+t+" WHERE memory_id=? AND revision=?", d["child"], d["rev"])
				}
				q.exec("DELETE FROM memory_dependencies WHERE child=? AND revision=?", d["child"], d["rev"])
			}
		}
		q.exec("DELETE FROM memory_reviews WHERE child=?", curr)
		q.exec("DELETE FROM memory_dependencies WHERE child=? OR parent=?", curr, curr)
		for _, src := range q.rows("SELECT * FROM memory_sources WHERE memory_id=?", curr) {
			quote := str(src["quote"])
			if quote == "" {
				continue
			}
			for _, r := range q.rows("SELECT s.memory_id FROM memory_sources s JOIN memories m ON m.id=s.memory_id WHERE s.message_id=? AND m.user_id=? AND (instr(s.quote,?)>0 OR instr(?,s.quote)>0)", src["message_id"], user, quote, quote) {
				todo = append(todo, str(r["memory_id"]))
			}
			for _, m := range q.rows(`SELECT m.* FROM messages m JOIN messages orig ON orig.id=? WHERE m.user_id=? AND (m.id=orig.id OR (m.agent_id=orig.agent_id AND m.session_id=orig.session_id AND m.project_id IS orig.project_id AND m.role=orig.role AND m.text=? AND NOT EXISTS(SELECT 1 FROM memory_sources s WHERE s.message_id=m.id)))`, src["message_id"], user, quote) {
				forgetSource(q, m, src)
				q.exec("DELETE FROM retain_admissions WHERE message_id=?", m["id"])
				q.exec("INSERT OR IGNORE INTO deleted_sources VALUES(?,?,?,?)", user, m["agent_id"], m["session_id"], m["source_id"])
				q.exec("UPDATE extraction_jobs SET state='cancelled',lease_token=NULL,result=NULL,error='user_deleted' WHERE message_id=?", m["id"])
			}
		}
		for _, table := range []string{"memory_sources", "memory_versions"} {
			q.exec("DELETE FROM "+table+" WHERE memory_id=?", curr)
		}
		q.exec("DELETE FROM memories WHERE id=? AND user_id=?", curr, user)
	}
	q.bump(user)
	q.event(user, "memory.deleted", id, Row{"deleted_memories": len(seen)})
	return Row{"memories": len(seen)}
}
