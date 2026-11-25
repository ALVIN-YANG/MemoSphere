package memory

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestReclassifyRetainsEvidenceAndVersion(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "categories.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.transaction(t.Context(), true, func(q tx) any {
		parent := save(q, "owner", "", Row{"text": "青林上次住临湖酒店", "category_id": "event", "entities": []Row{{"name": "青林", "kind": "place"}}})
		child := save(q, "owner", "", Row{"text": "可能更喜欢临湖", "category_id": "preference", "inputs": []Row{pick(parent, "id", "revision")}})
		revised := save(q, "owner", str(child["id"]), Row{"revision": 1, "category_id": "fact"})
		if revised["basis"] != "inference" || revised["state"] != "uncertain" || len(revised["inputs"].([]Row)) != 1 || revised["category_id"] != "fact" {
			t.Fatal("更改分类丢失推断依据")
		}
		q.exec("INSERT INTO memory_vectors VALUES(?,1,'fixture',X'FF')", parent["id"])
		q.exec("INSERT INTO memory_sources VALUES(?,1,1,'青林',0,2)", parent["id"])
		updated := save(q, "owner", str(parent["id"]), Row{"revision": 1, "category": "事实"})
		if updated["created_at"] != parent["created_at"] || len(updated["entities"].([]Row)) != 1 || len(updated["versions"].([]Row)) != 2 || q.count("SELECT count(*) FROM memory_vectors WHERE memory_id=? AND revision=2 AND hex(vector)='FF'", parent["id"]) != 1 || q.count("SELECT count(*) FROM memory_sources WHERE memory_id=? AND revision=2", parent["id"]) != 1 {
			t.Fatal("分类变更破坏版本、证据或向量")
		}
		previous := q.one("SELECT * FROM memories WHERE id=?", parent["id"])
		corrected := clone(previous)
		corrected["text"], corrected["revision"], corrected["last_operation"] = "实际住在山边", 3, "CORRECT"
		writeVersion(q, corrected, previous, "原记录有误", "CORRECT", nil)
		if q.count("SELECT count(*) FROM memory_versions WHERE memory_id=? AND disposition='invalidated'", parent["id"]) != 2 {
			t.Fatal("变更分类前的错误结论仍能作为事实使用")
		}

		return nil
	})
}
func TestCategoryMigrationIsAtomicAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.transaction(t.Context(), true, func(q tx) any {
		q.exec("DELETE FROM memory_schema_changes")
		for _, cat := range []string{"项目背景", "需求决策", "自选标签", "工具回执", "偏好"} {
			m := save(q, "a", "", Row{"text": "历史样本"})
			q.exec("UPDATE memories SET category=? WHERE id=?", cat, m["id"])
			q.exec("UPDATE memory_versions SET content=json_set(json_remove(content,'$.category_id'),'$.category',?) WHERE memory_id=?", cat, m["id"])
		}
		return nil
	})
	db.Close()
	for range 2 {
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		db.transaction(t.Context(), false, func(q tx) any {
			page := library(q, "a", url.Values{"category": {"fact"}})
			if num(page["total"]) != 1 || q.count("SELECT count(*) FROM memories WHERE category='未分类' AND revision=1") != 2 ||
				q.count("SELECT count(*) FROM memory_versions WHERE json_extract(content,'$.legacy_category') IN ('需求决策','自选标签')") != 2 ||
				q.count("SELECT count(*) FROM memories WHERE category='事件'") != 1 {
				t.Fatal("迁移结果不正确")
			}
			return nil
		})
		db.Close()
	}
}

// Copy a real legacy fixture read-only; compare immutable records and vector bytes across two opens.
func TestCategoryLegacyCopy(t *testing.T) {
	source := os.Getenv("MEMOSPHERE_LEGACY_TEST_COPY")
	if source == "" {
		t.Skip("optional local fixture")
	}
	uri := url.URL{Scheme: "file", Path: source, RawQuery: "mode=ro"}
	prior, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer prior.Close()
	path := filepath.Join(t.TempDir(), "rehearsal.sqlite")
	if _, err = prior.Exec("VACUUM INTO ?", path); err != nil {
		t.Fatal(err)
	}
	snapshot := func(db *sql.DB) []string {
		var result []string
		for _, query := range []string{
			"SELECT json_array(id,user_id,project_id,text,tags,state,basis,fact_time,created_at,updated_at,revision) FROM memories ORDER BY id",
			"SELECT json_array(memory_id,revision,json_remove(content,'$.category','$.category_id','$.legacy_category'),operation,disposition,recorded_at) FROM memory_versions ORDER BY memory_id,revision",
			"SELECT json_array(memory_id,revision,message_id,quote) FROM memory_sources ORDER BY memory_id,revision,message_id,quote",
			"SELECT json_array(child,revision,parent,parent_revision) FROM memory_dependencies ORDER BY child,revision,parent",
			"SELECT json_array(memory_id,revision,model,hex(vector)) FROM memory_vectors ORDER BY memory_id",
		} {
			rows, e := db.Query(query)
			if e != nil {
				t.Fatal(e)
			}
			for rows.Next() {
				var value string
				if e = rows.Scan(&value); e != nil {
					t.Fatal(e)
				}
				result = append(result, value)
			}
			if e = rows.Err(); e != nil {
				t.Fatal(e)
			}
			rows.Close()
		}
		return result
	}
	before := snapshot(prior)
	for range 2 {
		db, e := Open(path)
		if e != nil {
			t.Fatal(e)
		}
		if !slices.Equal(before, snapshot(db.DB)) {
			t.Fatal("migration changed evidence or index")
		}
		db.transaction(t.Context(), false, func(q tx) any {
			t.Logf("heads=%d versions=%d quotes=%d vectors=%d categories=%v", q.count("SELECT count(*) FROM memories"), q.count("SELECT count(*) FROM memory_versions"), q.count("SELECT count(*) FROM memory_sources"), q.count("SELECT count(*) FROM memory_vectors"), q.rows("SELECT category,count(*) n FROM memories GROUP BY category"))
			return nil
		})
		db.Close()
	}
}

func TestRejectUnknownCategoryAndInferredRule(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "reject.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var childID string
	db.transaction(t.Context(), true, func(q tx) any {
		parent := save(q, "a", "", Row{"text": "过去一次订房"})
		child := save(q, "a", "", Row{"text": "可能更喜欢湖景", "inputs": []Row{pick(parent, "id", "revision")}})
		childID = str(child["id"])
		return nil
	})
	for _, tc := range []struct {
		id string
		d  Row
	}{
		{"", Row{"text": "新建流程", "category_id": "procedure"}},
		{childID, Row{"revision": 1, "category_id": "rule"}},
		{childID, Row{"revision": 2, "category_id": "event"}},
	} {
		func() {
			defer func() {
				if p, ok := recover().(problem); !ok || p.Status != 400 {
					t.Errorf("expected rejection for %v", tc.d)
				}
			}()
			db.transaction(t.Context(), true, func(q tx) any { return save(q, "a", tc.id, tc.d) })
		}()
	}
}
