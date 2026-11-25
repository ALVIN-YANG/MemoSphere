package memory

import (
	"regexp"
	"strings"
)

// An embedded premise in a question is not testimony. A separate declarative
// clause in the same message can still provide evidence.
func interrogativeQuote(text, quote string) bool {
	found := false
	for _, sentence := range sentenceWithEnding.FindAllString(text, -1) {
		s := strings.TrimSpace(sentence)
		if !strings.Contains(s, quote) && !strings.Contains(s, strings.TrimRight(quote, "。！？!?")) {
			continue
		}
		found = true
		if !strings.HasSuffix(s, "？") && !strings.HasSuffix(s, "?") {
			return false
		}
		clauses := strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune("，,", r) })
		for _, part := range clauses[:max(0, len(clauses)-1)] {
			if strings.Contains(part, quote) && !inquiryCue.MatchString(part) {
				return false
			}
		}
	}
	return found
}

var sentenceWithEnding = regexp.MustCompile(`[^。！？!?]+[。！？!?]?`)
var inquiryCue = regexp.MustCompile(`(?:吗|是否|为什么|为何|哪|谁|怎么)`)
var companionBoundary = regexp.MustCompile(`[，,；;]\s*(?:这趟|此行|这次旅[行游]|这次出行)(?:我)?(?:和|跟|独自|自己|一个人)`)

func independentAdditions(raw string, ops []operation) []operation {
	var out []operation
	whole := companionBoundary.FindStringIndex(raw)
	for _, o := range ops {
		pos := companionBoundary.FindStringIndex(o.Quote)
		// A model may cite the booking clause but copy the companion from the
		// neighbouring sentence into its paraphrase. Keep each fact grounded.
		if whole != nil && pos == nil && o.Operation == "ADD" && o.Quote != "" && (strings.Contains(raw[:whole[0]], o.Quote) || strings.Contains(raw[whole[0]:], o.Quote)) {
			o.Text = o.Quote
		}
		if o.Operation != "ADD" || o.MemoryID != nil || len(o.Inputs) > 0 || o.Basis == "inference" || pos == nil || pos[0] == 0 {
			out = append(out, o)
			continue
		}
		parts := []string{o.Quote[:pos[0]], strings.TrimLeft(o.Quote[pos[0]:], "，,；; ")}
		for _, part := range parts {
			separate := o
			separate.Quote = part
			separate.Text = part
			if !strings.Contains(part, o.FactTime) {
				separate.FactTime = ""
			}
			if !strings.Contains(part, o.ValidSince) {
				separate.ValidSince = ""
			}
			out = append(out, separate)
		}
	}
	return out
}

var retractsEarlier = regexp.MustCompile(`(?:之前|先前|刚才|原来).{0,45}(?:说错|记错|不对|有误)`)
var givesReplacement = regexp.MustCompile(`其实|实际上|实际是|正确的是|应该是|更正为`)

// “其实是 X，之前说 Y 是说错了” supports X only if the positive clause
// survives with the retraction. Preserve the exact adjacent source span.
func correctionQuote(raw, quote string) string {
	if !retractsEarlier.MatchString(quote) || givesReplacement.MatchString(quote) || strings.Count(raw, quote) != 1 {
		return quote
	}
	start := strings.Index(raw, quote)
	before := strings.TrimRight(raw[:start], " \t")
	prefix := strings.TrimRight(before, "，,")
	if prefix == before {
		return quote
	}
	parts := strings.FieldsFunc(prefix, func(r rune) bool { return strings.ContainsRune("。！？!?，,；;", r) })
	if len(parts) == 0 {
		return quote
	}
	clause := parts[len(parts)-1]
	if !givesReplacement.MatchString(clause) || inquiryCue.MatchString(clause) || !strings.HasSuffix(prefix, clause) {
		return quote
	}
	expanded := raw[len(prefix)-len(clause) : start+len(quote)]
	if size(expanded) > 2000 || secret(expanded) {
		return quote
	}
	return expanded
}

var unchangedSubject = regexp.MustCompile(`([\p{Han}A-Za-z0-9-]{2,24})(?:没错|没变|不变|是对的|没有变化)`)
var companionPredicate = regexp.MustCompile(`同行|一起去|一个人|独自|(?:和|跟|带).{1,12}(?:去|出行|旅行)`)

// A change to the travelling party cannot retract an independent booking.
// Check the cited evidence rather than relying on a model-generated paraphrase.
func preservesNamedFact(q tx, old Row, raw string) bool {
	subjects := unchangedSubject.FindAllStringSubmatch(raw, -1)
	if len(subjects) == 0 || !companionPredicate.MatchString(raw) {
		return false
	}
	preserved := false
	for _, s := range q.rows("SELECT quote FROM memory_sources WHERE memory_id=? AND revision=?", old["id"], old["revision"]) {
		quote := str(s["quote"])
		if companionPredicate.MatchString(quote) {
			return false
		}
		for _, m := range subjects {
			preserved = preserved || strings.Contains(quote, m[1])
		}
	}
	return preserved
}
