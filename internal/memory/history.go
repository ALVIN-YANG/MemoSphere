package memory

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

type historyWindow struct {
	start, end string
	cutoff     *float64
}

var calendarDate = regexp.MustCompile(`(?:(\d{4})年)?([0-9一二三四五六七八九十]{1,3})月(?:([0-9一二三四五六七八九十廿]{1,3})[号日])?|\d{4}-\d{2}-\d{2}`)
var stateQuestion = regexp.MustCompile(`住哪|住在|住址|居住|工作单位|在哪.{0,3}(?:工作|上班)|职业|职位|就职|担任|当时.{0,5}(?:状态|负责|偏好)|已经知道|已经记得|当时知道`)
var vaguePast = regexp.MustCompile(`去年|前年|当时|以前|之前|上个月|上月|[0-9]{4}年`)
var knowledgeQuestion = regexp.MustCompile(`截至|截止|那时.{0,3}知道|当时.{0,3}知道|已经知道|当时.{0,3}有记录`)

func chineseNumber(s string) int {
	if n, e := strconv.Atoi(s); e == nil {
		return n
	}
	tens, ones := 0, 0
	for _, r := range s {
		if r == '十' {
			if tens != 0 {
				return 0
			}
			tens = max(1, ones) * 10
			ones = 0
			continue
		}
		if r == '廿' {
			tens = 20
			continue
		}
		n := strings.IndexRune("一二三四五六七八九", r)
		if n < 0 {
			return 0
		}
		ones = n/3 + 1
	}
	return tens + ones
}

// A month describes an uncertain start anywhere inside that month.
func calendarBounds(expr, stamp string) (string, string) {
	expr = strings.TrimSpace(expr)
	expr = strings.TrimSuffix(strings.TrimSuffix(expr, "以来"), "起")
	expr = strings.TrimPrefix(strings.TrimPrefix(expr, "从"), "自")
	if lo, hi := interval(expr, stamp); lo != "" && (datePat.MatchString(expr) || in(expr, []string{"昨天", "今天", "明天", "前天", "后天", "昨晚", "明早"})) {
		return lo, hi
	}
	t, ok := spokenTime(stamp)
	if expr == "现在" && ok {
		s := t.Format(time.DateOnly)
		return s, s
	}
	m := calendarDate.FindStringSubmatch(expr)
	if m == nil || m[0] != expr || m[2] == "" {
		return "", ""
	}
	year, _ := strconv.Atoi(m[1])
	if year == 0 {
		if !ok {
			return "", ""
		}
		year = t.Year()
	}
	month := chineseNumber(m[2])
	day := 1
	if m[3] != "" {
		day = chineseNumber(m[3])
	}
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if int(date.Month()) != month || date.Day() != day {
		return "", ""
	}
	end := date
	if m[3] == "" {
		end = date.AddDate(0, 1, -1)
	}
	return date.Format(time.DateOnly), end.Format(time.DateOnly)
}
func setValidStart(r Row, expr, stamp string) bool {
	lo, hi := calendarBounds(expr, stamp)
	if lo == "" {
		return false
	}
	r["valid_start"], r["valid_start_latest"] = lo, hi
	return true
}

var china = time.FixedZone("UTC+8", 8*3600)

func requestedHistory(d Row, today string) *historyWindow {
	v, k := str(d["valid_at"]), str(d["known_at"])
	check(d["valid_at"] == nil || v != "", "适用日期无效")
	check(d["known_at"] == nil || k != "", "知识截止时间无效")
	w := &historyWindow{}
	if v == "" && k == "" {
		query := str(d["query"])
		if d["purpose"] != "cite" || !stateQuestion.MatchString(query) || d["event_from"] != nil || d["event_to"] != nil {
			return nil
		}
		matches := calendarDate.FindAllString(query, -1)
		if len(matches) == 0 {
			if vaguePast.MatchString(query) {
				return w
			}
			return nil
		}
		if len(matches) != 1 {
			return w
		} // No single date can answer a comparison.
		anchor, _ := time.Parse(time.DateOnly, today)
		prefix := query[:strings.Index(query, matches[0])]
		for _, relative := range []struct {
			word string
			year int
		}{{"去年", -1}, {"前年", -2}, {"明年", 1}} {
			if strings.HasSuffix(prefix, relative.word) {
				anchor = anchor.AddDate(relative.year, 0, 0)
			}
		}
		w.start, w.end = calendarBounds(matches[0], anchor.Format(time.DateOnly)+"T12:00:00+08:00")
		if knowledgeQuestion.MatchString(query) {
			k = w.end
		}
	} else if v != "" {
		_, e := time.Parse(time.DateOnly, v)
		check(e == nil, "适用日期须为 YYYY-MM-DD")
		w.start, w.end = v, v
	}
	if k != "" {
		t, e := time.Parse(time.RFC3339Nano, k)
		if e != nil {
			t, e = time.ParseInLocation(time.DateOnly, k, china)
			check(e == nil, "知识截止时间格式无效")
			t = t.AddDate(0, 0, 1).Add(-time.Microsecond)
		}
		f := float64(t.UnixMicro()) / 1e6
		w.cutoff = &f
		if v == "" && w.start == "" {
			w.start = t.Format(time.DateOnly)
			w.end = w.start
		}
	}
	return w
}

// Read only versions with a supported start. The next visible change closes
// the version. Never infer a validity date from the ingestion clock.
func (w *historyWindow) candidates(q tx, a, d Row) {
	args := eligibleArgs(a, d)
	args = append(args, w.cutoff, w.cutoff, w.cutoff, w.cutoff, a["sensitive_access"], w.start, w.end, a["sensitive_access"])
	q.exec(`CREATE TEMP TABLE eligible AS WITH heads AS (SELECT * FROM memories WHERE `+eligibleWhere+`), versions AS (
 SELECT row_number() OVER (PARTITION BY m.id ORDER BY v.revision DESC) AS priority, m.id,m.user_id,m.project_id,v.revision,
 json_extract(v.content,'$.text') text,json_extract(v.content,'$.tags') tags,
 json_extract(v.content,'$.fact_time') fact_time,json_extract(v.content,'$.basis') basis,
 json_extract(v.content,'$.category') category,json_extract(v.content,'$.sensitive') sensitive,
 m.created_at,v.recorded_at updated_at,v.recorded_at known_from,n.recorded_at known_until,
 json_extract(v.content,'$.valid_start') valid_start,json_extract(v.content,'$.valid_start_latest') valid_start_latest,
 json_extract(n.content,'$.valid_start') valid_end,json_extract(n.content,'$.valid_start_latest') valid_end_latest, ? AS knowledge_cutoff
 FROM heads m JOIN memory_versions v ON v.memory_id=m.id
 LEFT JOIN memory_versions n ON n.memory_id=v.memory_id AND n.revision=v.revision+1 AND (? IS NULL OR n.recorded_at<=?)
 WHERE (v.recorded_at<=coalesce(?,v.recorded_at))
 AND v.disposition IN ('current','historical') AND json_extract(v.content,'$.state')='active'
 AND json_extract(v.content,'$.basis') IN ('user_statement','tool_observation')
 AND json_extract(v.content,'$.project_id') IS m.project_id AND (json_extract(v.content,'$.sensitive')=0 OR ?)
 AND json_extract(v.content,'$.valid_start_latest')<=?
 AND (n.revision IS NULL OR (json_extract(n.content,'$.valid_start')>? AND n.operation='UPDATE'
 AND json_extract(n.content,'$.project_id') IS m.project_id AND (json_extract(n.content,'$.sensitive')=0 OR ?)))) SELECT * FROM versions WHERE priority=1`, args...)
}
func (w *historyWindow) describe(r Row) Row {
	p := "day"
	if r["valid_start"] != r["valid_start_latest"] {
		p = "month"
	}
	var end any
	if r["valid_end"] == r["valid_end_latest"] {
		end = r["valid_end"]
	}
	return Row{"valid_to_earliest": r["valid_end"], "valid_to_latest": r["valid_end_latest"], "valid_from": r["valid_start"], "valid_from_latest": r["valid_start_latest"], "valid_to_exclusive": end, "precision": p, "known_from": r["known_from"], "known_until": r["known_until"]}
}
func sourceQuotes(q tx, a, r Row) []Row {
	if r["knowledge_cutoff"] == nil {
		return sources(q, a, r, " LIMIT 4")
	}
	sql := strings.Replace(sourceSQL, " ORDER BY", " AND m.recorded_at<=? ORDER BY", 1)
	return q.rows(sql+" LIMIT 4", r["id"], r["revision"], a["user_id"], a["agent_id"], a["sensitive_access"], a["global_access"], a["projects"], r["knowledge_cutoff"])
}
