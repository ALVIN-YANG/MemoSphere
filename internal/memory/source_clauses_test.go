package memory

import "testing"

func TestRetainedQuotesKeepIndependentAssertions(t *testing.T) {
	if !interrogativeQuote("我订这家店是因为能看海吗？", "我订这家店") || interrogativeQuote("我住杭州，明天是周几？", "我住杭州") {
		t.Fatal("question premise and independent assertion were conflated")
	}
	raw := "昨天订了星河客栈，这趟跟同学去。"
	parts := independentAdditions(raw, []operation{{Operation: "ADD", Text: raw, Quote: raw, FactTime: "昨天"}})
	if len(parts) != 2 || parts[1].FactTime != "" || parts[1].Quote != "这趟跟同学去。" {
		t.Fatal(parts)
	}
	cited := independentAdditions(raw, []operation{{Operation: "ADD", Text: raw, Quote: "昨天订了星河客栈"}})
	if cited[0].Text != cited[0].Quote {
		t.Fatal("paraphrase imported uncited companion")
	}
	q := "场地没变，其实是室内演出，之前说露天是说错了。"
	if got := correctionQuote(q, "之前说露天是说错了"); got != "其实是室内演出，之前说露天是说错了" {
		t.Fatal(got)
	}
}
