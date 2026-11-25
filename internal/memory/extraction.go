package memory

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type extraction struct {
	source, payload Row
	facts           map[string]Row
	modelIDs        map[string]string
	epoch           int64
	lease           string
	permissions     string
	admitted        []string
}

func epoch(q tx, u any) int64 {
	return q.count("SELECT coalesce(max(epoch),0) FROM memory_epochs WHERE user_id=?", u)
}
func grantFor(q tx, m Row) Row {
	g := q.one("SELECT a.* FROM agents a JOIN connections c ON a.token_hash=c.token_hash WHERE c.id=? AND c.user_id=? AND c.revoked_at IS NULL AND a.revoked=0", m["agent_id"], m["user_id"])
	allowed(g != nil && truth(g["write_access"]) && (!truth(m["sensitive"]) || truth(g["sensitive_access"])))
	scope(g, m["project_id"])
	return g
}
func snapshot(q tx, m Row, lease string) extraction {
	g := grantFor(q, m)
	data := pick(m, "id", "role", "project_id", "sensitive", "recorded_at", "spoken_at")
	raw := str(m["text"])
	data["text"] = truncated(redact(raw), 24000)
	facts := map[string]Row{}
	modelIDs := map[string]string{}
	shown := []Row{}
	recent := []Row{}
	summary := []Row{}
	if truth(g["read_access"]) {
		terms := keywords(raw)
		rank := "0"
		args := []any{m["user_id"], m["project_id"], g["global_access"], g["sensitive_access"]}
		for _, t := range terms {
			rank += "+(instr(lower(text||category),?)>0)"
			args = append(args, t)
		}
		for _, f := range q.rows("SELECT * FROM memories WHERE user_id=? AND state!='suspended' AND basis!='inference' AND (project_id IS ? OR (project_id IS NULL AND ?)) AND (sensitive=0 OR ?) ORDER BY ("+rank+") DESC,updated_at DESC LIMIT 100", args...) {
			facts[str(f["id"])] = f
			item := decodeMemory(f)
			if identifierAssignment(q, f) {
				item["identifier_assignment"] = true
			}
			localID := "M" + strconv.Itoa(len(shown)+1)
			modelIDs[localID] = str(f["id"])
			item["id"] = localID
			delete(item, "user_id")
			delete(item, "tags")
			for k, v := range item {
				if t, ok := v.(string); ok {
					item[k] = redact(t)
				}
			}
			shown = append(shown, item)
		}
		args = []any{m["user_id"], m["agent_id"], m["session_id"], m["project_id"], g["sensitive_access"]}
		cond := "user_id=? AND agent_id=? AND session_id=? AND project_id IS ? AND (sensitive=0 OR ?)"
		recent = q.rows("SELECT role,text,recorded_at FROM messages WHERE "+cond+" AND id<? AND content_state='raw' ORDER BY id DESC LIMIT 6", append(args, m["id"])...)
		slices.Reverse(recent)
		for _, r := range recent {
			r["text"] = truncated(redact(str(r["text"])), 2000)
		}
		for _, r := range q.rows("SELECT summary FROM conversation_streams WHERE "+cond, args...) {
			summary = append(summary, visible(q, r, truth(g["sensitive_access"]))...)
		}
	}
	at := time.UnixMicro(int64(m["recorded_at"].(float64) * 1e6)).UTC().Format(time.RFC3339Nano)
	var admitted []string
	if rec := q.one("SELECT spans FROM retain_admissions WHERE message_id=?", m["id"]); rec != nil {
		var spans [][2]int
		must(json.Unmarshal([]byte(str(rec["spans"])), &spans))
		admitted = []string{}
		for _, span := range spans {
			ensure(span[0] >= 0 && span[1] >= span[0] && span[1] <= len(raw), "stale_snapshot")
			admitted = append(admitted, raw[span[0]:span[1]])
		}
	}
	return extraction{m, Row{"source": data, "recent": recent, "conversation_summary": summary, "recorded_at_utc": at, "global_allowed": truth(g["global_access"]), "memories": shown, "prompt_version": "memory-2", "source_truncated": size(raw) > 24000}, facts, modelIDs, epoch(q, m["user_id"]), lease, encoded(permissions(g)), admitted}
}

type operation struct {
	Operation                                  string
	MemoryID                                   *string `json:"memory_id"`
	Text, Scope, Kind, Category, Reason, Quote string
	FactTime                                   string          `json:"fact_time"`
	ValidSince                                 string          `json:"valid_since"`
	Mentions                                   []entityMention `json:"entity_mentions"`
	Entities                                   []entityMention
	Tags, Inputs                               []string
	Sensitive                                  bool
	Basis                                      string
}

func link(q tx, r, m Row, quote string) {
	start, end := offsets(str(m["text"]), quote)
	q.exec("INSERT OR IGNORE INTO memory_sources(memory_id,revision,message_id,quote,source_start,source_end) VALUES(?,?,?,?,?,?)", r["id"], r["revision"], m["id"], quote, start, end)
}

var subjectRE = regexp.MustCompile(`([A-Za-z0-9_-]+|[㐀-鿿]{2})(酒店|公司|大学|医院|大厦)`)

func wrongSubject(target, raw string, facts map[string]Row) bool {
	targets := subjectRE.FindAllStringSubmatch(target, -1)
	names := map[string][]string{}
	for _, v := range targets {
		names[v[2]] = append(names[v[2]], strings.ToLower(v[1]))
	}
	for _, f := range facts {
		for _, v := range subjectRE.FindAllStringSubmatch(str(f["text"]), -1) {
			group := names[v[2]]
			if len(group) == 0 || !strings.Contains(strings.ToLower(raw), strings.ToLower(v[1])) {
				continue
			}
			matched := false
			for _, n := range group {
				matched = matched || strings.Contains(strings.ToLower(raw), n)
			}
			if !matched {
				return true
			}
		}
	}
	return false
}

// A correction may name the destination without asserting a new order number.
var referenceCode = regexp.MustCompile(`(?i)\b[a-z]{1,12}-[a-z0-9]*[0-9][a-z0-9-]*\b`)

var comparisonRE = regexp.MustCompile(`帮我.{0,6}(挑|选|比较)|挑一[家个种]|选哪[家个种]|二选一`)
var assertionRE = regexp.MustCompile(`记住|记一下|我(一向|习惯|偏好|喜欢|通常)|以后|其实|说错|更正`)
var retrospectiveRE = regexp.MustCompile(`我.{0,6}(之前|以前|上次|明确).{0,8}(说过|提过)|我.{0,4}(说过|提过).{0,18}[吗？?]`)
var personalRE = regexp.MustCompile(`我(现在|决定|喜欢|已经|确认|住|要)`)
var complexRE = regexp.MustCompile(`[，,；;？?]|[。.!].+`)
var measureRE = regexp.MustCompile(`([0-9]+([.][0-9]+)?|[零一二两三四五六七八九十百千]+)\s*(分钟|小时|公里|千米|米|元|天|毫秒|秒)`)
var selfRE = regexp.MustCompile(`我|习惯|一向|以后|偏好|喜欢|决定|说错|不对`)

func transient(raw, quote string) bool {
	if !strings.ContainsAny(raw, "?？") || assertionRE.MatchString(raw) {
		return false
	}
	for _, c := range strings.FieldsFunc(raw, func(r rune) bool { return strings.ContainsRune("。！？!?；;", r) }) {
		if selfRE.MatchString(c) || !(strings.Contains(c, strings.Trim(quote, "。！？!?；;")) || strings.Contains(quote, c)) || !strings.ContainsAny(c, "，,、而比别") {
			continue
		}
		seen := map[string]bool{}
		for _, m := range measureRE.FindAllStringSubmatch(c, -1) {
			if seen[m[3]] {
				return true
			}
			seen[m[3]] = true
		}
	}
	return false
}
func questionOnly(raw string) bool {
	if comparisonRE.MatchString(raw) && !assertionRE.MatchString(raw) {
		return true
	}
	i := retrospectiveRE.FindStringIndex(raw)
	t := strings.TrimSpace(raw)
	return i != nil && (strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？")) && !strings.ContainsAny(t, "。！!；;") && !personalRE.MatchString(raw[:i[0]])
}
func applyOp(q tx, snap extraction, g Row, o operation, touched map[string]bool) string {
	m := snap.source
	src := str(snap.payload["source"].(Row)["text"])
	var old Row
	if o.Operation == "CORRECT" && m["role"] == "user" && o.Quote != "" {
		o.Quote = correctionQuote(src, o.Quote)
	}
	ensure(in(o.Operation, []string{"ADD", "UPDATE", "CORRECT", "MERGE", "IGNORE", "UNCERTAIN"}) && in(o.Scope, []string{"global", "project"}) && in(o.Kind, []string{"project", "profile"}) && size(o.Text) <= 4000 && size(o.Quote) <= 2000 && size(o.Reason) <= 240 && size(o.Category) <= 40 && size(o.FactTime) <= 120 && len(o.Tags) <= 20 && len(o.Entities)+len(o.Mentions) <= 16 && len(o.Inputs) <= 12, "invalid_plan")
	items := entityItems(append(o.Mentions, o.Entities...), o.Quote)
	if o.MemoryID != nil {
		id := *o.MemoryID
		if stable, ok := snap.modelIDs[id]; ok {
			id = stable
		}
		old = snap.facts[id]
		ensure(old != nil, "unknown_memory")
		if in(o.Operation, []string{"MERGE", "UPDATE", "CORRECT"}) && identifierAssignment(q, old) && !reassignsIdentifier(o.Quote, items) {
			if o.Operation == "CORRECT" {
				return "IGNORE"
			}
			// Store only the new utterance. The model's combined text can include the
			// earlier assignment, which remains independently retractable.
			o.Operation, o.MemoryID, o.Text, old = "ADD", nil, o.Quote, nil
		}
		if o.Operation == "CORRECT" && (wrongSubject(str(old["text"]), src, snap.facts) || preservesNamedFact(q, old, src)) {
			return "IGNORE"
		}
	}
	if m["role"] == "user" && o.Quote != "" && (transient(src, o.Quote) || interrogativeQuote(src, o.Quote)) {
		return "IGNORE"
	}
	if old != nil {
		scope(g, old["project_id"])
		allowed(truth(g["read_access"]) && (truth(g["sensitive_access"]) || !truth(old["sensitive"])))
		id := str(old["id"])
		ensure(!touched[id], "repeated_target")
		touched[id] = true
	}
	if o.Operation == "IGNORE" {
		if old != nil && m["role"] == "user" && o.Quote != "" && strings.Contains(src, o.Quote) {
			link(q, old, m, o.Quote)
		}
		return "IGNORE"
	}
	o.Text = strings.TrimSpace(o.Text)
	for _, code := range referenceCode.FindAllString(o.Text, -1) {
		if !strings.Contains(strings.ToUpper(o.Quote), strings.ToUpper(code)) {
			o.Text = o.Quote
			break
		}
	}
	ensure(o.Text != "" && strings.TrimSpace(o.Quote) != "" && strings.Contains(src, o.Quote), "unsupported_evidence")
	ensure(!secret(o.Text) && !secret(o.Quote), "credential_memory")
	for _, t := range o.Tags {
		ensure(size(t) <= 40, "invalid_tags")
	}
	ensure(o.FactTime == "" || strings.Contains(src, o.FactTime) || old["fact_time"] == o.FactTime, "invented_time")
	ensure(!in(o.Operation, []string{"UPDATE", "CORRECT", "MERGE"}) || old != nil, "missing_target")
	ensure(o.Operation != "ADD" || old == nil, "add_has_target")
	p := m["project_id"]
	if old != nil {
		p = old["project_id"]
	} else if o.Scope == "global" && !(p != nil && hasReference(items)) {
		p = nil
	}
	scope(g, p)
	allowed(old != nil || p != nil || o.Scope == "global")
	role := str(m["role"])
	if role != "user" {
		ensure(o.Kind == "project" && p != nil && old == nil, "untrusted_author")
	}
	basis := map[string]string{"user": "user_statement", "assistant": "inference", "tool": "tool_observation"}[role]
	ensure(o.Basis == "" || o.Basis == "inference" || o.Basis == basis, "basis_role_mismatch")
	ts := now()
	r := Row{"id": uuid.NewString(), "user_id": m["user_id"], "project_id": p, "text": o.Text, "category": o.Category, "tags": encoded(o.Tags), "sensitive": flag(truth(m["sensitive"]) || o.Sensitive || phonePattern.MatchString(o.Text) || truth(old["sensitive"])), "fact_time": o.FactTime, "revision": 1, "created_at": ts, "updated_at": ts, "state": "active", "origin": "automatic", "kind": o.Kind, "last_operation": o.Operation, "basis": basis}
	if old != nil {
		r["id"], r["revision"], r["created_at"], r["kind"] = old["id"], num(old["revision"])+1, old["created_at"], old["kind"]
	}
	if o.Operation == "UNCERTAIN" || role == "assistant" {
		r["state"] = "uncertain"
	}
	refs := []Row{}
	if o.Basis == "inference" || len(o.Inputs) > 0 {
		ensure(old == nil && len(o.Inputs) > 0 && o.Operation == "ADD" && truth(g["read_access"]), "invalid_inference")
		for _, id := range o.Inputs {
			if stable, ok := snap.modelIDs[id]; ok {
				id = stable
			}
			f := snap.facts[id]
			ensure(f != nil, "unknown_input")
			scope(g, f["project_id"])
			allowed(!truth(f["sensitive"]) || truth(g["sensitive_access"]))
			refs = append(refs, pick(f, "id", "revision"))
		}
		r["basis"], r["state"] = "inference", "uncertain"
	}
	c := categoryOf(o.Category)
	ensure(c.ID != "" && (c.ID != "rule" || r["basis"] != "inference"), "invalid_category")
	r["category"] = c.Label
	if o.ValidSince != "" && strings.Contains(o.Quote, o.ValidSince) {
		setValidStart(r, o.ValidSince, str(m["spoken_at"]))
	}
	if old != nil && (o.Operation == "UPDATE" || o.Operation == "MERGE") {
		prior := q.one("SELECT json_extract(content,'$.valid_start') start,json_extract(content,'$.valid_start_latest') latest FROM memory_versions WHERE memory_id=? AND revision=?", old["id"], old["revision"])
		if o.Operation == "MERGE" && r["valid_start"] == nil && prior["start"] != nil {
			r["valid_start"], r["valid_start_latest"] = prior["start"], prior["latest"]
		}
		if latest := str(r["valid_start_latest"]); o.Operation == "UPDATE" && latest != "" && latest < str(prior["start"]) {
			// A newly arrived, older effective date cannot replace a newer current
			// state. Keep the competing assertion and its source for owner review.
			old = nil
			r["id"], r["revision"], r["created_at"], r["state"], r["last_operation"] = uuid.NewString(), 1, ts, "uncertain", "UNCERTAIN"
			o.Operation = "UNCERTAIN"
		}
	}
	if truth(g["read_access"]) && truth(g["sensitive_access"]) {
		protectReference(q, r, items)
	}
	if old == nil && len(o.Inputs) == 0 && o.Basis != "inference" {
		dup := q.one("SELECT m.id,m.revision FROM memories m JOIN memory_sources s ON s.memory_id=m.id AND s.revision=m.revision WHERE s.message_id=? AND s.quote=? AND m.project_id IS ? AND m.basis=? AND m.sensitive=? AND m.state='active'", m["id"], o.Quote, p, basis, r["sensitive"])
		if dup == nil {
			dup = q.one("SELECT id,revision FROM memories WHERE user_id=? AND project_id IS ? AND text=? AND state='active' AND basis=? AND sensitive=?", m["user_id"], p, o.Text, basis, r["sensitive"])
		}
		if dup != nil {
			if role != "assistant" {
				link(q, dup, m, o.Quote)
			}
			return "IGNORE"
		}
	}
	writeVersion(q, r, old, o.Reason, o.Operation, refs)
	bindEntities(q, r, items, o.Quote, truth(g["read_access"]), truth(g["sensitive_access"]))
	if o.Operation == "CORRECT" && truth(g["read_access"]) && entityReuseEnabled() {
		correctedReference(q, r, old, o.Quote, truth(g["sensitive_access"]))
	}
	if old != nil && o.Operation == "MERGE" {
		q.exec("INSERT OR IGNORE INTO memory_sources SELECT memory_id,?,message_id,quote,source_start,source_end FROM memory_sources WHERE memory_id=? AND revision=?", r["revision"], old["id"], old["revision"])
	}
	link(q, r, m, o.Quote)
	q.event(str(m["user_id"]), "memory.organized", str(r["id"]), Row{"operation": o.Operation, "revision": r["revision"]})
	return o.Operation
}
func (s *Service) process(ctx context.Context, snap extraction, used *int64) {
	raw := str(snap.payload["source"].(Row)["text"])
	var ops []operation
	quotes := snap.admitted
	if quotes != nil {
		snap.payload["accepted_quotes"] = quotes
	}
	if snap.source["role"] != "user" || !questionOnly(raw) {
		if quotes == nil && snap.source["role"] != "tool" && complexRE.MatchString(raw) {
			res, id := s.call(ctx, snap.source["id"], "extraction", admissionPrompt, Row{"source": snap.payload["source"]})
			*used = id
			var a struct{ Quotes []string }
			strict(res, &a, "invalid_admission")
			ensure(a.Quotes != nil && len(a.Quotes) <= 12, "invalid_admission")
			for _, v := range a.Quotes {
				ensure(strings.TrimSpace(v) != "" && strings.Contains(raw, v), "invalid_admission")
			}
			quotes = a.Quotes
			snap.payload["accepted_quotes"] = quotes
			status := "evidence_selected"
			if len(quotes) == 0 {
				status = "no_durable_evidence"
			}
			s.outcome(id, status)
			*used = 0
		}
		if quotes == nil || len(quotes) > 0 {
			res, id := s.call(ctx, snap.source["id"], "extraction", extractionInstructions(), snap.payload)
			*used = id
			if res["type"] == "json_object" {
				delete(res, "type")
			}
			var plan struct{ Operations []json.RawMessage }
			strict(res, &plan, "invalid_plan")
			ensure(plan.Operations != nil && len(plan.Operations) <= 12, "invalid_plan")
			ignored := true
			for _, v := range plan.Operations {
				o := operation{Kind: "profile", Scope: "project", Reason: "自动整理", Tags: []string{}}
				dec := json.NewDecoder(strings.NewReader(string(v)))
				dec.DisallowUnknownFields()
				ensure(dec.Decode(&o) == nil, "invalid_plan")
				if o.Operation != "IGNORE" {
					ignored = false
					if quotes != nil {
						ensure(slices.ContainsFunc(quotes, func(q string) bool { return strings.Contains(q, o.Quote) }), "quote_outside_admission")
					}
				}
				ops = append(ops, o)
			}
			if len(quotes) > 1 {
				for _, accepted := range quotes {
					covered := slices.ContainsFunc(ops, func(o operation) bool {
						return o.Quote != "" && (strings.Contains(accepted, o.Quote) || strings.Contains(o.Quote, accepted))
					})
					ensure(covered, "unaccounted_evidence")
				}
			}
			if ignored && snap.source["role"] == "tool" && size(raw) <= 240 && strings.TrimSpace(raw) != "" && !secret(raw) && snap.source["project_id"] != nil {
				ops = []operation{{Operation: "ADD", Scope: "project", Kind: "project", Text: raw, Quote: raw, Category: "event", Basis: "tool_observation", Tags: []string{}}}
			}
		}
	}
	s.DB.transaction(ctx, true, func(q tx) any {
		m := snap.source
		ensure(q.one("SELECT 1 FROM extraction_jobs WHERE message_id=? AND state='running' AND lease_token=?", m["id"], snap.lease) != nil && epoch(q, m["user_id"]) == snap.epoch, "stale_snapshot")
		g := grantFor(q, m)
		ensure(encoded(permissions(g)) == snap.permissions, "stale_snapshot")
		counts := map[string]int{}
		touched := map[string]bool{}
		if m["role"] == "user" {
			ops = independentAdditions(raw, ops)
		}
		sharedMentions(ops)
		for _, op := range ops {
			counts[applyOp(q, snap, g, op, touched)]++
		}
		q.exec("DELETE FROM retain_admissions WHERE message_id=?", m["id"])
		q.exec("UPDATE extraction_jobs SET state='done',lease_token=NULL,error=NULL,result=?,updated_at=? WHERE message_id=?", encoded(Row{"operations": counts, "source_truncated": snap.payload["source_truncated"]}), now(), m["id"])
		return nil
	})
	s.outcome(*used, "applied")
}
