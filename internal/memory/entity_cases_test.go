package memory

import (
	"strings"
	"testing"
)

func TestEntityAcrossConversations(t *testing.T) {
	const booked = "杭州湖畔酒店这次的订单号是 HB-17"
	const late = "HB-17 这单我想延迟退房"
	const other = "苏州湖畔酒店这次的订单号是 SZ-29"
	const correction = "延迟退房说的是苏州那家"
	var target string
	s, key, p := workerFixture(t, func(prompt string, data Row) Row {
		msg := str(data["source"].(map[string]any)["text"])
		op := Row{"operation": "ADD", "category": "fact", "quote": msg, "text": msg}
		switch msg {
		case booked, other:
			city, code := "杭州", "HB-17"
			if msg == other {
				city, code = "苏州", "SZ-29"
			}
			op["entities"] = []Row{{"kind": "place", "name": "湖畔酒店", "qualifier": city}, {"kind": "order", "name": code}}
		case late:
			// The real extractor occasionally proposes merging the new request into
			// the booking. The booking and its independently correctable request survive.
			op["operation"], op["memory_id"] = "MERGE", "M1"
			op["entities"] = []Row{{"kind": "order", "name": "HB-17"}}
		case correction:
			op["operation"] = "CORRECT"
			op["memory_id"] = target
		}
		return Row{"operations": []Row{op}}
	})
	h := s.mcpHandler()
	ctx := t.Context()
	record := func(source, msg string) Row {
		return caseCall(t, h, key, "retain", Row{"project_id": p, "session_id": source, "messages": []Row{{"source_id": source, "role": "user", "text": msg}}})
	}
	add := func(source, msg string) {
		record(source, msg)
		if !s.extract(ctx) {
			t.Fatal("missing extraction")
		}
	}
	find := func(names ...string) Row {
		return caseCall(t, h, key, "recall", Row{"project_id": p, "entity_names": names, "terms": []string{}, "purpose": "cite"})
	}
	add("a", booked)
	add("b", late)
	add("c", other)
	r := objects(find("杭州湖畔酒店")["memories"])
	if len(r) != 2 {
		t.Fatalf("C39: %v", r)
	}
	for _, item := range r {
		if len(texts(item["quotes"])) != 1 {
			t.Fatal("source not preserved")
		}
		if item["text"] == late {
			target = str(item["id"])
		}
	}
	if target == "" {
		t.Fatal("late checkout missing")
	}
	s.DB.transaction(ctx, true, func(q tx) any {
		bindings := entities(q, target, 1)
		if len(bindings) != 2 || bindings[0]["id"] == bindings[1]["id"] {
			t.Fatal("order and hotel must differ", bindings)
		}
		for _, user := range []string{"owner", "unrelated"} {
			hidden := project(q, user, "隐藏项目")
			save(q, user, "", Row{"project_id": hidden["id"], "text": "隐藏酒店记录", "entities": []Row{{"name": "湖畔酒店", "kind": "place", "qualifier": "隐藏城市"}}})
		}
		return nil
	})
	ambiguous := find("湖畔酒店")["retrieval"].(map[string]any)
	candidates := objects(ambiguous["unresolved"])
	if ambiguous["status"] != "needs_clarification" || len(candidates) != 1 || len(texts(candidates[0]["entity_ids"])) != 2 || strings.Contains(encoded(candidates), "隐藏") {
		t.Fatal("C40 scope", ambiguous)
	}
	add("d", correction)
	if r = objects(find("杭州湖畔酒店")["memories"]); len(r) != 1 || r[0]["text"] != booked {
		t.Fatal("C41 old object", r)
	}
	if r = objects(find("苏州湖畔酒店")["memories"]); len(r) != 2 {
		t.Fatal("C41 new object", r)
	}
	s.DB.transaction(ctx, false, func(q tx) any {
		vs := detail(q, "owner", target)["versions"].([]Row)
		if len(vs) != 2 || vs[1]["disposition"] != "invalidated" || len(vs[1]["entities"].([]Row)) != 2 {
			t.Fatal("C41 binding history", vs)
		}
		return nil
	})
	record("b", late)
	if s.extract(ctx) || len(objects(find("杭州湖畔酒店")["memories"])) != 1 {
		t.Fatal("C41 duplicate delivery restored old binding")
	}
}

func TestReferenceVisibilityAndForgetting(t *testing.T) {
	s, key, p := workerFixture(t, nil)
	ctx := t.Context()
	var source, public, private string
	s.DB.transaction(ctx, true, func(q tx) any {
		withItems := func(text string, sensitive bool, mentions []Row, readSecrets bool) string {
			r := save(q, "owner", "", Row{"text": text, "project_id": p, "sensitive": sensitive})
			// save returns the owner's decoded detail; direct binding uses the stored row.
			m := q.one("SELECT * FROM memories WHERE id=?", r["id"])
			bindEntities(q, m, mentions, text, true, readSecrets)
			return str(m["id"])
		}
		ref := Row{"name": "F-17", "kind": "order"}
		withItems("成都栖云酒店订单 F-17", true, []Row{ref, {"name": "栖云酒店", "qualifier": "成都", "kind": "place"}}, true)
		public = withItems("F-17 已备注", false, []Row{ref}, false)
		if len(entities(q, public, 1)) != 1 {
			t.Fatal("a restricted writer resolved a sensitive hotel")
		}
		// Use the same pre-publication sensitivity decision as automatic extraction.
		r := Row{"user_id": "owner", "project_id": p, "sensitive": 0}
		protectReference(q, r, []Row{ref})
		if !truth(r["sensitive"]) {
			t.Fatal("sensitive booking was not protected")
		}
		private = withItems("由 F-17 申请开票", true, []Row{ref}, true)
		if q.count("SELECT count(*) FROM current_entity_links WHERE memory_id=? AND kind='place'", private) != 0 {
			// Two identities now carry F-17. The ambiguous reference must stay unbound.
			t.Fatal("permission partition collapse")
		}
		erase(q, "owner", public)
		// Other isolated F-17 records can remain ambiguous. Use a unique qualified
		// booking to check exact removal of proved links.
		hotel := Row{"name": "海湾酒店", "qualifier": "宁波", "kind": "place"}
		uniqueRef := Row{"name": "T-55", "kind": "order"}
		source = withItems("宁波海湾酒店 T-55", true, []Row{hotel, uniqueRef}, true)
		private = withItems("T-55 次日抵达", true, []Row{uniqueRef}, true)
		if len(entities(q, private, 1)) != 2 {
			t.Fatal("missing derived hotel")
		}
		erase(q, "owner", source)
		if len(entities(q, private, 1)) != 1 || q.count("SELECT count(*) FROM entity_bindings WHERE proof_id=?", source) != 0 {
			t.Fatal("forgotten evidence retained a link")
		}
		return nil
	})
	h := s.mcpHandler()
	data := caseCall(t, h, key, "recall", Row{"purpose": "cite", "terms": []string{}, "entity_names": []string{"宁波海湾酒店"}, "project_id": p})
	if len(objects(data["memories"])) != 0 || strings.Contains(encoded(data), "T-55") {
		t.Fatal("restricted info in recall", data)
	}
}
