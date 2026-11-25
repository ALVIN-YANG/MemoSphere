package memory

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const eligibleWhere = "user_id=? AND (project_id IS ? OR (project_id IS NULL AND ?)) AND (sensitive=0 OR ?) AND state='active' AND basis IN ('user_statement','tool_observation')"

func eligibleArgs(a, d Row) []any {
	return []any{a["user_id"], d["project_id"], a["global_access"], a["sensitive_access"]}
}

var tokensPat = regexp.MustCompile(`[a-z0-9]+(?:[._+#-][a-z0-9]+)*|[㐀-鿿]+`)
var wordPat = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_.+-]{1,30}`)
var hanPat = regexp.MustCompile(`[一-鿿]+`)

func grams(word string) []string {
	r := []rune(word)
	if len(r) < 2 {
		return []string{word}
	}
	v := []string{}
	for i := 0; i < len(r)-1; i++ {
		v = append(v, string(r[i:i+2]))
	}
	return v
}
func keywords(s string) []string {
	rs := []rune(s)
	s = string(rs[:min(4000, len(rs))])
	words := wordPat.FindAllString(strings.ToLower(s), -1)
	for _, v := range hanPat.FindAllString(s, -1) {
		if size(v) > 1 {
			words = append(words, grams(v)...)
		}
	}
	a := []string{}
	for _, v := range unique(words) {
		if !in(v, []string{"我的", "现在", "这个", "我们", "可以", "需要", "已经", "之前", "实际", "请把", "一下", "的是"}) {
			a = append(a, v)
		}
	}
	return a[:min(24, len(a))]
}
func tokens(s string) []string {
	out := []string{}
	for _, w := range tokensPat.FindAllString(normalized(s), -1) {
		a := []string{w}
		if []rune(w)[0] > 127 {
			a = grams(w)
		}
		for _, x := range a {
			out = append(out, "t"+hex.EncodeToString([]byte(x)))
		}
	}
	return out
}
func day(expr string, stamp any) Row {
	t, ok := spokenTime(str(stamp))
	if !ok {
		return nil
	}
	for _, x := range []struct {
		key    string
		offset int
	}{{"前天", -2}, {"昨晚", -1}, {"昨天", -1}, {"今天", 0}, {"明天", 1}, {"明早", 1}, {"后天", 2}} {
		if strings.Contains(expr, x.key) {
			return Row{"date": t.AddDate(0, 0, x.offset).Format(time.DateOnly), "expression": x.key}
		}
	}
	return nil
}

var datePat = regexp.MustCompile(`^(\d{4})[-年](\d{1,2})(?:[-月](\d{1,2}))?(?:[日号月])?(?:晚间|晚上|晚|上午|下午)?$`)

func interval(s string, stamp any) (string, string) {
	if m := datePat.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		y, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		d := 1
		if m[3] != "" {
			d, _ = strconv.Atoi(m[3])
		}
		t := time.Date(y, time.Month(month), d, 0, 0, 0, 0, time.UTC)
		if y < 1 || int(t.Month()) != month || t.Day() != d {
			return "", ""
		}
		end := t
		if m[3] == "" {
			end = t.AddDate(0, 1, -1)
		}
		return t.Format(time.DateOnly), end.Format(time.DateOnly)
	}
	if d := day(s, stamp); d != nil {
		return str(d["date"]), str(d["date"])
	}
	return "", ""
}

const sourceSQL = `SELECT m.id message_id,m.agent_id,m.role,m.spoken_at,m.sensitive,s.quote FROM memory_sources s JOIN messages m ON m.id=s.message_id WHERE s.memory_id=? AND s.revision=? AND m.user_id=? AND m.agent_id=? AND (m.sensitive=0 OR ?) AND ((m.project_id IS NULL AND ?) OR m.project_id IN (SELECT value FROM json_each(?))) ORDER BY m.id`

func sources(q tx, a, r Row, limit string) []Row {
	return q.rows(sourceSQL+limit, r["id"], r["revision"], a["user_id"], a["agent_id"], a["sensitive_access"], a["global_access"], a["projects"])
}
func explain(q tx, a, r, d Row) Row {
	i := pick(r, "id", "revision", "text", "basis", "fact_time", "created_at", "updated_at")
	i["category_id"] = categoryOf(str(r["category"])).ID
	i["spoken_at"] = nil
	srcs := sourceQuotes(q, a, r)
	quotes := []string{}
	for _, x := range srcs {
		quotes = append(quotes, redact(str(x["quote"])))
		if str(r["fact_time"]) != "" && strings.Contains(str(x["quote"]), str(r["fact_time"])) && i["event_day"] == nil {
			if v := day(str(r["fact_time"]), x["spoken_at"]); v != nil {
				i["event_day"] = v
			}
		}
	}
	if len(srcs) > 0 {
		i["spoken_at"] = srcs[0]["spoken_at"]
	}
	if d["purpose"] == "cite" {
		if r["valid_start"] == nil && truth(d["include_context"]) && len(srcs) > 0 && r["basis"] == "user_statement" {
			src := srcs[0]
			peers := q.rows(`SELECT f.id,f.revision,s.quote,m.text utterance FROM memory_sources s JOIN memories f ON f.id=s.memory_id AND f.revision=s.revision JOIN messages m ON m.id=s.message_id WHERE s.message_id=? AND f.id<>? AND f.user_id=? AND f.project_id IS ? AND f.state='active' AND f.basis='user_statement' AND (f.sensitive=0 OR ?) AND m.role='user'`, src["message_id"], r["id"], a["user_id"], r["project_id"], a["sensitive_access"])
			contextQuotes := []Row{}
			causal := ""
			for _, p := range peers {
				s1, s2, u := str(src["quote"]), str(p["quote"]), str(p["utterance"])
				for _, pair := range [][2]string{{s1, s2}, {s2, s1}} {
					event, motive := pair[0], pair[1]
					if causal != "" || event == "" || strings.ContainsRune("。！？!?；;", []rune(event)[size(event)-1]) || !(strings.HasPrefix(motive, "因为") || strings.HasPrefix(motive, "由于") || strings.HasPrefix(motive, "为了")) || strings.Count(u, event) != 1 || strings.Count(u, motive) != 1 {
						continue
					}
					end := strings.Index(u, event) + len(event)
					start := strings.Index(u, motive)
					if start >= end && strings.Trim(u[end:start], " ,，") == "" {
						causal = redact(u[end-len(event) : start+len(motive)])
					}
				}
				x, y := strings.Index(u, s1), strings.Index(u, s2)
				if x >= 0 && y >= x+len(s1) && strings.Trim(u[x+len(s1):y], " ，,。；;、") == "" && (len(contextQuotes) == 0 || size(s2) > size(str(contextQuotes[0]["quote"]))) {
					contextQuotes = []Row{{"id": p["id"], "revision": p["revision"], "quote": redact(s2)}}
				}
			}
			if causal != "" && !in(causal, quotes) {
				quotes = append([]string{causal}, quotes...)
			}
			i["context_quotes"] = contextQuotes
		}
		i["quotes"] = quotes
	}
	return i
}
func (s *Service) recall(ctx context.Context, key string, d Row) Row {
	purpose := str(d["purpose"])
	check(in(purpose, []string{"skip", "cite", "select"}), "无效的召回意图")
	terms := texts(d["terms"])
	check(len(terms) <= 12 && size(str(d["query"])) <= 4000, "检索词过长")
	for _, pair := range []struct {
		k string
		n int
	}{{"terms", 80}, {"entity_ids", 80}, {"entity_names", 80}} {
		v := texts(d[pair.k])
		check(len(v) <= 12, "检索参数过多")
		for _, x := range v {
			check(size(x) <= pair.n && (pair.k == "terms" || strings.TrimSpace(x) != ""), "检索参数无效")
		}
	}
	terms = []string{}
	for _, x := range texts(d["terms"]) {
		if v := strings.ToLower(strings.TrimSpace(x)); v != "" {
			terms = append(terms, v)
		}
	}
	terms = unique(terms)
	today := time.Now().Format(time.DateOnly)
	for _, k := range []string{"as_of", "event_from", "event_to"} {
		if d[k] != nil {
			t, e := time.Parse(time.DateOnly, str(d[k]))
			check(e == nil, "时间格式无效")
			if k == "as_of" {
				today = t.Format(time.DateOnly)
			}
		}
	}
	query := str(d["query"])
	if d["event_from"] == nil && d["event_to"] == nil && strings.Contains(query, "上周") {
		t, _ := time.Parse(time.DateOnly, today)
		end := t.AddDate(0, 0, -((int(t.Weekday())+6)%7 + 1))
		d["event_to"] = end.Format(time.DateOnly)
		d["event_from"] = end.AddDate(0, 0, -6).Format(time.DateOnly)
	}
	check(d["event_from"] == nil || d["event_to"] == nil || str(d["event_from"]) <= str(d["event_to"]), "事件时间范围无效")
	period := requestedHistory(d, today)
	var vector []float32
	if period == nil && s.Vectors != nil && !s.Lexical && purpose != "skip" && (strings.TrimSpace(query) != "" || len(terms) > 0) {
		s.DB.transaction(ctx, false, func(q tx) any { return checkedAgent(q, key, d, false) })
		x := query
		if strings.TrimSpace(x) == "" {
			x = strings.Join(terms, " ")
		}
		if !s.Vectors.External() || (!truth(d["local_only"]) && !secret(x)) {
			vector, _ = s.Vectors.Search(ctx, x)
		}
	}
	return s.DB.transaction(ctx, false, func(q tx) any {
		a := checkedAgent(q, key, d, false)
		out := Row{"purpose": purpose, "memories": []Row{}}
		if purpose == "skip" {
			return out
		}
		if period != nil && os.Getenv("MEMOSPHERE_HISTORICAL_READ") == "off" {
			out["retrieval"] = Row{"mode": "historical", "status": "disabled", "degraded": []string{"historical_disabled"}}
			return out
		}
		if !s.Lexical || period != nil {
			return s.hybrid(q, a, d, terms, vector, today, out, period)
		}
		check(len(texts(d["entity_ids"]))+len(texts(d["entity_names"])) == 0 && d["event_from"] == nil && d["event_to"] == nil, "词法对照模式不支持结构化筛选")
		expanded := unique(append(append(append([]string{}, terms...), keywords(query)...), keywords(strings.Join(terms, " "))...))
		out["retrieval"] = Row{"mode": "lexical"}
		if len(expanded) == 0 {
			return out
		}
		score := strings.TrimSuffix(strings.Repeat("(instr(lower(text || ' ' || tags),?)>0)+", len(expanded)), "+")
		args := []any{}
		for _, t := range expanded {
			args = append(args, t)
		}
		args = append(args, eligibleArgs(a, d)...)
		// Keep the literal-match baseline bounded to five rows, using the same category weights.
		args = append(args, categoryOf("rule").Label, categoryFactor(purpose, categoryOf("rule").Label), categoryOf("preference").Label, categoryFactor(purpose, categoryOf("preference").Label))
		rows := q.rows("SELECT *,("+score+") AS relevance FROM memories WHERE "+eligibleWhere+" AND relevance>0 ORDER BY relevance*(CASE category WHEN ? THEN ? WHEN ? THEN ? ELSE 1 END) DESC,updated_at DESC,id LIMIT 5", args...)
		items := []Row{}
		for _, r := range rows {
			items = append(items, explain(q, a, r, d))
		}
		out["memories"] = items
		return out
	}).(Row)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func order(scores map[string]float64) []string {
	ids := keys(scores)
	sort.SliceStable(ids, func(i, j int) bool { return scores[ids[i]] > scores[ids[j]] })
	return ids
}

var recentRE = regexp.MustCompile(`(?i)最近|近期|最新|这几天|recent|latest`)
var progressRE = regexp.MustCompile(`(?i)进度|进展|动态|情况|状态|更新|办得|订得|progress|status|update`)

func (s *Service) hybrid(q tx, a, d Row, terms []string, vector []float32, today string, out Row, period *historyWindow) Row {
	if period != nil {
		period.candidates(q, a, d)
	} else {
		q.exec("CREATE TEMP TABLE eligible AS SELECT * FROM memories WHERE "+eligibleWhere, eligibleArgs(a, d)...)
	}
	defer q.exec("DROP TABLE eligible")
	q.exec("CREATE UNIQUE INDEX temp.eligible_id ON eligible(id)")
	qualifiedNames := map[string]map[string]bool{}
	labels := map[string]map[string]bool{}
	links := map[string]map[string]bool{}
	link := func(id, eid string, names []string) {
		if links[id] == nil {
			links[id] = map[string]bool{}
		}
		links[id][eid] = true
		if labels[eid] == nil {
			labels[eid] = map[string]bool{}
		}
		for _, n := range names {
			labels[eid][normalized(n)] = true
		}
	}
	for _, r := range q.rows("SELECT e.* FROM current_entity_links e JOIN eligible m ON m.id=e.memory_id AND m.revision=e.revision") {
		if label, qualifier := normalized(str(r["name"])), normalized(str(r["qualifier"])); qualifier != "" {
			if qualifiedNames[label] == nil {
				qualifiedNames[label] = map[string]bool{}
			}
			qualifiedNames[label][qualifier] = true
		}
		link(str(r["memory_id"]), str(r["entity_id"]), append([]string{str(r["name"])}, texts(parseJSON(str(r["aliases"])))...))
	}
	for _, r := range q.rows("SELECT m.id,p.id pid,p.name FROM eligible m JOIN projects p ON p.id=m.project_id AND p.user_id=m.user_id") {
		link(str(r["id"]), str(r["pid"]), []string{str(r["name"])})
	}
	requiredIDs := texts(d["entity_ids"])
	unresolved := []Row{}
	status := "no_match"
	for _, name := range queryEntityNames(d, qualifiedNames) {
		matches := []string{}
		for _, id := range keys(labels) {
			if labels[id][normalized(name)] {
				matches = append(matches, id)
			}
		}
		matches = qualifiedMatches(matches, labels, str(d["query"]), name)
		if len(matches) == 1 {
			requiredIDs = append(requiredIDs, matches[0])
		} else {
			unresolved = append(unresolved, Row{"name": name, "entity_ids": matches, "options": entityOptions(matches, labels)})
			if len(matches) > 1 {
				status = "needs_clarification"
			}
		}
	}
	if len(unresolved) > 0 {
		out["retrieval"] = Row{"mode": "hybrid", "status": status, "unresolved": unresolved, "degraded": []string{}}
		return out
	}
	rows := map[string]Row{}
	dates := map[string][2]string{}
	query := str(d["query"])
	if period != nil && strings.Contains(query, "住") {
		terms = append(terms, "搬到", "居住", "住在", "住址")
	}
	words := unique(tokens(strings.Join(append([]string{query}, terms...), " ")))
	words = words[:min(len(words), 160)]
	stamps := map[string]any{}
	for _, r := range q.rows(`SELECT m.id,msg.spoken_at FROM eligible m JOIN memory_sources s ON m.id=s.memory_id AND m.revision=s.revision JOIN messages msg ON msg.id=s.message_id WHERE msg.user_id=? AND msg.agent_id=? AND (msg.sensitive=0 OR ?) AND msg.spoken_at IS NOT NULL AND ((msg.project_id IS NULL AND ?) OR msg.project_id IN (SELECT value FROM json_each(?))) AND m.fact_time<>'' AND instr(s.quote,m.fact_time)>0 ORDER BY msg.id`, a["user_id"], a["agent_id"], a["sensitive_access"], a["global_access"], a["projects"]) {
		id := str(r["id"])
		if stamps[id] == nil {
			stamps[id] = r["spoken_at"]
		}
	}
	start, end := str(d["event_from"]), str(d["event_to"])
	expectedVectors := 0
	for _, r := range q.rows("SELECT * FROM eligible") {
		id := str(r["id"])
		ok := true
		for _, eid := range requiredIDs {
			if !links[id][eid] {
				ok = false
			}
		}
		lo, hi := interval(str(r["fact_time"]), stamps[id])
		if start != "" || end != "" {
			ok = ok && lo != "" && (start == "" || hi >= start) && (end == "" || lo <= end)
		}
		if ok {
			rows[id] = r
			if s.Vectors == nil || !s.Vectors.External() || !truth(r["sensitive"]) {
				expectedVectors++
			}
			dates[id] = [2]string{lo, hi}
		}
	}
	ranks := map[string]map[string]int{}
	add := func(channel string, ids []string) {
		for idx, id := range ids[:min(30, len(ids))] {
			if ranks[id] == nil {
				ranks[id] = map[string]int{}
			}
			ranks[id][channel] = idx + 1
		}
	}
	if len(words) > 0 && len(rows) > 0 {
		q.exec("CREATE VIRTUAL TABLE temp.corpus USING fts5(id UNINDEXED, body, names)")
		defer q.exec("DROP TABLE corpus")
		for _, id := range keys(rows) {
			r := rows[id]
			names := []string{}
			for _, eid := range keys(links[id]) {
				names = append(names, keys(labels[eid])...)
			}
			q.exec("INSERT INTO corpus VALUES(?,?,?)", id, strings.Join(tokens(str(r["text"])+" "+strings.Join(texts(parseJSON(str(r["tags"]))), " ")), " "), strings.Join(tokens(strings.Join(names, " ")), " "))
		}
		ids := []string{}
		for _, r := range q.rows("SELECT id FROM corpus WHERE corpus MATCH ? ORDER BY bm25(corpus,0,1,1),id LIMIT 30", strings.Join(words, " OR ")) {
			ids = append(ids, str(r["id"]))
		}
		add("bm25", ids)
	}
	hits := map[string]bool{}
	for eid, names := range labels {
		if in(eid, requiredIDs) {
			continue
		}
		for name := range names {
			if in(name, terms) || size(name) > 1 && strings.Contains(normalized(query), name) {
				hits[eid] = true
			}
		}
	}
	counts := map[string]float64{}
	for id := range rows {
		for eid := range links[id] {
			if hits[eid] {
				counts[id]++
			}
		}
	}
	add("entity", order(counts))
	cosine := map[string]float64{}
	indexed := 0
	anchor := false
	if vector != nil {
		for _, r := range q.rows("SELECT v.memory_id,v.vector FROM memory_vectors v JOIN eligible m ON m.id=v.memory_id AND m.revision=v.revision WHERE v.model=?", s.Vectors.ID()) {
			id := str(r["memory_id"])
			b, _ := r["vector"].([]byte)
			if rows[id] == nil || len(b) != len(vector)*4 {
				continue
			}
			indexed++
			var dot float64
			for j, x := range vector {
				dot += float64(x) * float64(math.Float32frombits(binary.LittleEndian.Uint32(b[j*4:])))
			}
			if dot >= 0.38 {
				cosine[id] = dot
				if dot >= 0.42 {
					anchor = true
				}
			}
		}
		if !anchor {
			clear(cosine)
		}
		add("semantic", order(cosine))
	}
	structured := (period != nil || len(requiredIDs) > 0 || start != "" || end != "") && strings.TrimSpace(query) == "" && len(terms) == 0
	if structured {
		for id := range rows {
			if ranks[id] == nil {
				ranks[id] = map[string]int{}
			}
		}
	}
	progress := recentRE.MatchString(query) && progressRE.MatchString(query)
	if d["recent_progress"] != nil {
		progress = truth(d["recent_progress"])
	}
	current, _ := time.Parse(time.DateOnly, today)
	scores := map[string]float64{}
	traces := map[string]Row{}
	matchingPreference := false
	for id, positions := range ranks {
		base := 0.
		for _, k := range []string{"bm25", "entity", "semantic"} {
			if p := positions[k]; p > 0 {
				base += 1 / float64(60+p)
			}
		}
		factor := 1.
		delta := dates[id]
		if progress && delta[0] != "" && delta[0] == delta[1] && delta[0] <= today && rows[id]["category"] != categoryOf("preference").Label {
			dt, _ := time.Parse(time.DateOnly, delta[0])
			factor += 0.15 * math.Exp2(-current.Sub(dt).Hours()/24/14)
		}
		weight := 1.0
		if s.categoryComparison == "" {
			weight = categoryFactor(str(d["purpose"]), str(rows[id]["category"]))
		}
		scores[id] = base * factor * weight
		trace := Row{"ranks": positions, "rrf": base, "category_factor": weight, "time_factor": factor, "score": scores[id]}
		if v, ok := cosine[id]; ok {
			trace["cosine"] = v
		}
		if delta[0] != "" {
			trace["event_interval"] = delta
			trace["event_precision"] = "interval"
			if delta[0] == delta[1] {
				trace["event_precision"] = "day"
			}
		}
		traces[id] = trace
		if rows[id]["category"] == "偏好" && len(positions) > 0 {
			matchingPreference = true
		}
	}
	items := []Row{}
	for _, id := range order(scores) {
		if s.categoryComparison == "hard" && matchingPreference && d["purpose"] == "select" && (rows[id]["category"] != "偏好" || len(ranks[id]) == 0) {
			continue
		}
		i := explain(q, a, rows[id], d)
		if period != nil {
			i["validity"] = period.describe(rows[id])
		}
		i["ranking"] = traces[id]
		i["entity_ids"] = keys(links[id])
		items = append(items, i)
		if len(items) == 5 {
			break
		}
	}
	if len(items) > 0 {
		status = "matched"
	}
	degraded := []string{}
	var model any
	if period != nil {
		degraded = append(degraded, "historical_lexical")
	} else if vector == nil && (query != "" || len(terms) > 0) {
		degraded = append(degraded, "semantic_unavailable")
	} else if indexed < expectedVectors && !structured {
		degraded = append(degraded, "semantic_index_incomplete")
	}
	if vector != nil {
		model = s.Vectors.ID()
	}
	out["retrieval"] = Row{"mode": "hybrid", "status": status, "model": model, "degraded": degraded}
	out["memories"] = items
	return out
}
