package memory

import (
	"bytes"
	"encoding/json"
	"github.com/google/uuid"
	"os"
	"strings"
)

type entityMention struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Qualifier string   `json:"qualifier"`
	Aliases   []string `json:"aliases"`
}

func (m *entityMention) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		m.Kind = "other"
		return json.Unmarshal(raw, &m.Name)
	}
	type fields entityMention
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode((*fields)(m))
}

func entityItems(mentions []entityMention, quote string) []Row {
	items := []Row{}
	for _, m := range mentions {
		if m.Name == "" || size(m.Name) > 80 || !strings.Contains(quote, m.Name) {
			continue
		}
		qualifier := m.Qualifier
		if !strings.Contains(quote, qualifier) {
			qualifier = ""
		}
		aliases := []string{}
		for _, alias := range m.Aliases {
			if alias != "" && strings.Contains(quote, alias) {
				aliases = append(aliases, alias)
			}
		}
		if m.Kind == "" {
			m.Kind = "other"
		}
		// A qualified full name and a separate city are two spellings of one name.
		if qualifier != "" && strings.HasPrefix(m.Name, qualifier) && len(m.Name) > len(qualifier) {
			aliases = append(aliases, m.Name)
			m.Name = strings.TrimPrefix(m.Name, qualifier)
		}
		items = append(items, Row{"name": m.Name, "kind": m.Kind, "aliases": unique(aliases), "qualifier": qualifier})
	}
	return items
}

const bindingCandidates = `SELECT e.*,m.sensitive FROM current_entity_links e JOIN memories m ON m.id=e.memory_id AND m.revision=e.revision
 WHERE m.user_id=? AND m.project_id IS ? AND m.sensitive<=? AND m.state='active' AND m.basis IN ('user_statement','tool_observation') AND e.kind=?`

func entityReuseEnabled() bool     { return os.Getenv("MEMOSPHERE_ENTITY_REUSE") != "off" }
func isReference(kind string) bool { return kind == "order" || kind == "reference" }
func labelsOf(e Row) []string      { return append(texts(parseJSON(str(e["aliases"]))), str(e["name"])) }

func bindEntities(q tx, r Row, items []Row, evidence string, allowRead, sensitiveAccess bool) {
	allowRead = allowRead && entityReuseEnabled()
	check(len(items) <= 32, "实体过多")
	direct := map[string]bool{}
	refs := []string{}
	for _, e := range items {
		name, kind := strings.TrimSpace(str(e["name"])), str(val(e, "kind", "other"))
		qualifier := strings.TrimSpace(str(e["qualifier"]))
		aliases := texts(e["aliases"])
		check(len(aliases) <= 8, "别名过多")
		values := append([]string{name, kind}, aliases...)
		if qualifier != "" {
			values = append(values, qualifier)
		}
		for _, s := range values {
			check(size(strings.TrimSpace(s)) > 0 && size(s) <= 80 && !secret(s), "实体格式无效")
			check(truth(r["sensitive"]) || !phonePattern.MatchString(s), "实体包含敏感信息")
		}
		if evidence != "" && !strings.Contains(evidence, name) {
			continue
		}
		if qualifier != "" {
			if evidence != "" && !strings.Contains(evidence, qualifier) {
				qualifier = ""
			} else if !strings.HasPrefix(name, qualifier) {
				aliases = unique(append(aliases, qualifier+name))
			}
		}
		id := str(e["id"])
		if id != "" {
			u, err := uuid.Parse(id)
			check(err == nil, "实体 ID 需使用 UUID")
			id = u.String()
		} else {
			candidates := map[string]bool{}
			if allowRead && (qualifier != "" || isReference(kind)) {
				for _, v := range q.rows(bindingCandidates, r["user_id"], r["project_id"], flag(sensitiveAccess && truth(r["sensitive"])), kind) {
					if (!isReference(kind) || qualifier != "") && normalized(qualifier) != normalized(str(v["qualifier"])) {
						continue
					}
					for _, n := range labelsOf(v) {
						if normalized(n) == normalized(name) {
							candidates[str(v["entity_id"])] = true
						}
					}
				}
			}
			if len(candidates) == 1 {
				id = keys(candidates)[0]
			} else {
				values := []any{r["user_id"], r["project_id"], r["id"], normalized(kind), normalized(name)}
				if qualifier != "" {
					values = append(values, normalized(qualifier))
				}
				parts := []string{}
				for _, v := range values {
					parts = append(parts, asciiJSON(v))
				}
				id = uuid.NewSHA1(uuid.NameSpaceURL, []byte("["+strings.Join(parts, ", ")+"]")).String()
			}
		}
		q.exec("INSERT OR REPLACE INTO memory_entities(memory_id,revision,entity_id,name,kind,aliases) VALUES(?,?,?,?,?,?)", r["id"], r["revision"], id, name, kind, encoded(aliases))
		if qualifier != "" {
			q.exec("INSERT OR REPLACE INTO entity_bindings(memory_id,revision,entity_id,qualifier) VALUES(?,?,?,?)", r["id"], r["revision"], id, qualifier)
		}
		direct[kind] = true
		if isReference(kind) {
			refs = append(refs, id)
		}
	}
	if !allowRead || r["last_operation"] == "CORRECT" || len(refs) != 1 || len(direct) != 1 {
		return
	}
	// One identified order can locate its explicitly co-mentioned object. An
	// inferred link cannot serve as the source of another inferred link.
	rows := q.rows(`SELECT DISTINCT peer.* FROM current_entity_links ref
 JOIN memories m ON m.id=ref.memory_id AND m.revision=ref.revision
 JOIN current_entity_links peer ON peer.memory_id=ref.memory_id AND peer.revision=ref.revision
 WHERE ref.entity_id=? AND m.user_id=? AND m.project_id IS ? AND m.sensitive<=? AND m.basis IN ('user_statement','tool_observation') AND m.state='active'
 AND peer.kind NOT IN ('order','reference','other') AND peer.proof_id IS NULL AND peer.qualifier<>'' ORDER BY peer.memory_id,peer.revision`, refs[0], r["user_id"], r["project_id"], flag(sensitiveAccess && truth(r["sensitive"])))
	bindUniquePeer(q, r, rows)
}
func bindUniquePeer(q tx, r Row, rows []Row) {
	ids := map[string]bool{}
	for _, v := range rows {
		ids[str(v["entity_id"])] = true
	}
	if len(ids) != 1 {
		return
	}
	v := rows[0]
	if q.count("SELECT count(*) FROM memory_entities WHERE memory_id=? AND revision=? AND entity_id=?", r["id"], r["revision"], v["entity_id"]) > 0 {
		return
	}
	q.exec("INSERT OR IGNORE INTO memory_entities(memory_id,revision,entity_id,name,kind,aliases) VALUES(?,?,?,?,?,?)", r["id"], r["revision"], v["entity_id"], v["name"], v["kind"], v["aliases"])
	q.exec("INSERT OR REPLACE INTO entity_bindings VALUES(?,?,?,?,?,?)", r["id"], r["revision"], v["entity_id"], v["qualifier"], v["memory_id"], v["revision"])
}

// Resolve an explicit correction such as “苏州那家” only against the same
// type and name as the corrected binding. The qualifier must be in the quote.
func correctedReference(q tx, r, old Row, quote string, sensitiveAccess bool) {
	if !strings.Contains(quote, "那家") && !strings.Contains(quote, "那间") {
		return
	}
	matches := []Row{}
	for _, prior := range entities(q, old["id"], old["revision"]) {
		if str(prior["qualifier"]) == "" {
			continue
		}
		for _, candidate := range q.rows(bindingCandidates, r["user_id"], r["project_id"], flag(sensitiveAccess && truth(r["sensitive"])), prior["kind"]) {
			loc := str(candidate["qualifier"])
			if loc != "" && candidate["proof_id"] == nil && candidate["name"] == prior["name"] && candidate["entity_id"] != prior["id"] && strings.Contains(quote, loc) {
				matches = append(matches, candidate)
			}
		}
	}
	bindUniquePeer(q, r, matches)
}

// Only a genuinely ambiguous name triggers implicit narrowing. Unambiguous
// mentions retain the normal entity ranking in open-ended questions.
func queryEntityNames(d Row, qualified map[string]map[string]bool) []string {
	names := texts(d["entity_names"])
	if len(names) > 0 || len(texts(d["entity_ids"])) > 0 || d["purpose"] != "cite" {
		return names
	}
	query := normalized(str(d["query"]))
	for _, label := range keys(qualified) {
		if len(qualified[label]) > 1 && size(label) > 1 && strings.Contains(query, label) {
			names = append(names, label)
		}
	}
	return names
}
func qualifiedMatches(ids []string, labels map[string]map[string]bool, query, name string) []string {
	if len(ids) < 2 {
		return ids
	}
	matching := []string{}
	for _, id := range ids {
		for label := range labels[id] {
			if label != normalized(name) && strings.Contains(label, normalized(name)) && strings.Contains(normalized(query), label) {
				matching = append(matching, id)
				break
			}
		}
	}
	if len(matching) > 0 {
		return matching
	}
	return ids
}
func entityOptions(ids []string, labels map[string]map[string]bool) []Row {
	options := []Row{}
	for _, id := range ids {
		options = append(options, Row{"id": id, "names": keys(labels[id])})
	}
	return options
}

// Protect the new fact before publishing a link to a sensitive order. A
// restricted or write-only connection never searches the sensitive partition.
func protectReference(q tx, r Row, items []Row) {
	if truth(r["sensitive"]) {
		return
	}
	for _, e := range items {
		if !isReference(str(e["kind"])) {
			continue
		}
		for _, prior := range q.rows(bindingCandidates, r["user_id"], r["project_id"], 1, e["kind"]) {
			if normalized(str(e["name"])) == normalized(str(prior["name"])) && truth(prior["sensitive"]) {
				r["sensitive"] = 1
				return
			}
		}
	}
}

// The extractor can split one utterance into several facts. Carry a qualified
// name across those facts only when its full spelling occurs in this quote.
func sharedMentions(ops []operation) {
	for i := range ops {
		names := map[string]bool{}
		for _, m := range append(append([]entityMention{}, ops[i].Entities...), ops[i].Mentions...) {
			names[m.Name] = true
		}
		for j := range ops {
			if i == j {
				continue
			}
			for _, m := range append(append([]entityMention{}, ops[j].Entities...), ops[j].Mentions...) {
				if names[m.Name] || m.Name == "" || m.Qualifier == "" || isReference(m.Kind) {
					continue
				}
				full := m.Qualifier + m.Name
				if strings.HasPrefix(m.Name, m.Qualifier) {
					full = m.Name
				}
				if strings.Contains(ops[i].Quote, full) && strings.Contains(ops[j].Quote, full) && len(ops[i].Entities)+len(ops[i].Mentions) < 16 {
					ops[i].Mentions = append(ops[i].Mentions, m)
					names[m.Name] = true
				}
			}
		}
	}
}

// An identifier assignment and a later request about that identifier are
// independently correctable facts. Do not merge a checkout or invoice into
// the record establishing which entity owns an order.
func identifierAssignment(q tx, record Row) bool {
	return q.count(`SELECT EXISTS(SELECT 1 FROM current_entity_links e
 JOIN current_entity_links peer ON peer.memory_id=e.memory_id AND peer.revision=e.revision
 WHERE e.memory_id=? AND e.revision=? AND e.kind IN ('order','reference')
 AND peer.kind NOT IN ('order','reference','other') AND peer.proof_id IS NULL)`, record["id"], record["revision"]) > 0
}

func hasReference(items []Row) bool {
	for _, item := range items {
		if isReference(str(item["kind"])) {
			return true
		}
	}
	return false
}

func reassignsIdentifier(quote string, items []Row) bool {
	if !hasReference(items) {
		return false
	}
	// A grounded object and its reference must both be named. Merely mentioning
	// an order number never authorizes replacing its identity record.
	for _, item := range items {
		if !isReference(str(item["kind"])) && item["kind"] != "other" && str(item["qualifier"]) != "" && strings.Contains(quote, str(item["name"])) {
			return true
		}
	}
	return false
}
