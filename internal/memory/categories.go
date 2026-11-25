package memory

import (
	"encoding/json"
	assets "memosphere/memosphere"
	"strings"
)

// IDs are the integration contract. Existing clients can still use the Chinese label.
type memoryCategory struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

var categoryCatalog = func() []memoryCategory {
	raw, err := assets.Console().Open("category-catalog.json")
	must(err)
	defer raw.Close()
	var catalog []memoryCategory
	must(json.NewDecoder(raw).Decode(&catalog))
	return catalog
}()

const pendingCategory = "未分类"

func categoryOf(value string) memoryCategory {
	for _, c := range categoryCatalog {
		if value == c.ID || value == c.Label {
			return c
		}
	}
	return memoryCategory{}
}

func legacyLabel(raw, basis string) string {
	c := categoryOf(raw)
	if c.ID == "rule" && basis != "user_statement" && basis != "tool_observation" {
		return pendingCategory
	}
	if c.ID != "" {
		return c.Label
	}
	switch raw {
	case "个人背景", "项目背景", "背景事实":
		return categoryOf("fact").Label
	case "工具回执":
		return categoryOf("event").Label
	default:
		return pendingCategory
	}
}

func migrateCategories(q tx) {
	q.exec("CREATE TABLE IF NOT EXISTS memory_schema_changes (name TEXT PRIMARY KEY)")
	if q.count("SELECT count(*) FROM memory_schema_changes WHERE name='categories_v1'") != 0 {
		return
	}
	// The temporary map also covers arbitrary user-created labels. Scan each record once.
	q.exec("CREATE TEMP TABLE category_map(raw TEXT,basis TEXT,label TEXT,id TEXT,PRIMARY KEY(raw,basis)) WITHOUT ROWID")
	for _, row := range q.rows(`SELECT category AS raw,basis FROM memories UNION
 SELECT coalesce(json_extract(content,'$.category'),''),coalesce(json_extract(content,'$.basis'),'') FROM memory_versions`) {
		raw, basis := str(row["raw"]), str(row["basis"])
		label := legacyLabel(raw, basis)
		q.exec("INSERT INTO category_map VALUES(?,?,?,?)", raw, basis, label, categoryOf(label).ID)
	}
	q.exec(`UPDATE memories AS m SET legacy_category=m.category,category=c.label FROM category_map c
 WHERE m.category=c.raw AND m.basis=c.basis AND m.category<>c.label`)
	q.exec(`UPDATE memory_versions AS v SET content=json_set(v.content,'$.legacy_category',json_extract(v.content,'$.category'),'$.category',c.label,'$.category_id',c.id)
 FROM category_map c WHERE c.raw=coalesce(json_extract(v.content,'$.category'),'') AND c.basis=coalesce(json_extract(v.content,'$.basis'),'')
 AND json_type(v.content,'$.category_id') IS NULL`)
	q.exec("DROP TABLE category_map")
	q.exec("INSERT INTO memory_schema_changes VALUES('categories_v1')")
}

func writeCategory(d, old Row) string {
	raw, supplied := d["category_id"]
	if !supplied {
		raw, supplied = d["category"]
	}
	if !supplied {
		if old != nil {
			return str(old["category"])
		}
		return categoryCatalog[0].Label
	}
	c := categoryOf(str(raw))
	if d["category_id"] == nil && c.ID == "" {
		// Unambiguous retired labels can still arrive from older clients.
		c = categoryOf(legacyLabel(str(raw), "user_statement"))
		if old != nil && old["legacy_category"] != nil && str(raw) == str(old["legacy_category"]) {
			return str(old["category"])
		}
	}
	if d["category_id"] != nil && d["category"] != nil {
		check(categoryOf(str(d["category"])).ID == c.ID, "分类不一致")
	}
	if c.ID == "" && old != nil && old["category"] == pendingCategory && (raw == pendingCategory || raw == "") {
		return pendingCategory
	}
	check(c.ID != "", "请选择记忆目录中的分类")
	return c.Label
}

func extractionInstructions() string {
	return strings.ReplaceAll(extractionPrompt, "{{categories}}", encoded(categoryCatalog))
}

// A label edit creates an auditable revision without converting a quote or inference
// into a new statement by the owner. Existing evidence continues to refer to its version.
func reclassify(q tx, old Row, label string) Row {
	id, rev := old["id"], old["revision"]
	user := str(old["user_id"])
	check(label != "规则" || old["basis"] == "user_statement" || old["basis"] == "tool_observation", "规则需要直接依据")
	if old["category"] == label {
		return detail(q, user, str(id))
	}
	r := clone(old)
	r["category"], r["revision"], r["updated_at"], r["last_operation"] = label, num(rev)+1, now(), "RECLASSIFY"
	prior := q.one("SELECT content FROM memory_versions WHERE memory_id=? AND revision=?", id, rev)
	if prior != nil {
		for k, v := range parseJSON(str(prior["content"])).(map[string]any) {
			if _, current := r[k]; !current {
				r[k] = v
			}
		}
	}
	vector := q.one("SELECT model,vector FROM memory_vectors WHERE memory_id=? AND revision=?", id, rev)
	writeVersion(q, r, old, "本人调整分类", "RECLASSIFY", inputs(q, id, rev))
	for table, fields := range map[string]string{"memory_sources": "message_id,quote,source_start,source_end", "memory_entities": "entity_id,name,kind,aliases", "entity_bindings": "entity_id,qualifier,proof_id,proof_revision"} {
		q.exec("INSERT INTO "+table+"(memory_id,revision,"+fields+") SELECT memory_id,?,"+fields+" FROM "+table+" WHERE memory_id=? AND revision=?", r["revision"], id, rev)
	}
	q.exec(`UPDATE entity_bindings SET proof_revision=? WHERE proof_id=? AND proof_revision=? AND EXISTS (SELECT 1 FROM memories m WHERE m.id=entity_bindings.memory_id AND m.revision=entity_bindings.revision)`, r["revision"], id, rev)
	if vector != nil {
		q.exec("INSERT INTO memory_vectors VALUES(?,?,?,?)", id, r["revision"], vector["model"], vector["vector"])
	}
	if old["state"] == "suspended" {
		q.exec("INSERT OR REPLACE INTO memory_reviews(child,revision) VALUES(?,?)", id, r["revision"])
	}
	q.event(user, "memory.updated", str(id), Row{"operation": "RECLASSIFY", "revision": r["revision"]})
	return detail(q, user, str(id))
}
