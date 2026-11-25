package memory

import (
	"context"
	"github.com/tiktoken-go/tokenizer"
	"sync"
)

var recallCodec = sync.OnceValues(func() (tokenizer.Codec, error) { return tokenizer.Get(tokenizer.Cl100kBase) })

// Budget the actual serialized evidence rows (including quotes and provenance).
// Token accounting uses a fixed offline encoding, independent of the embedding provider.
func (s *Service) recallForClient(ctx context.Context, key string, d Row) Row {
	if d["purpose"] == nil {
		d["purpose"] = "cite"
	}
	raw := val(d, "max_tokens", 4096)
	budget := num(raw)
	check(sameRevision(raw, budget) && budget >= 0 && budget <= 65536, "max_tokens 需在 0 到 65536 之间")
	r := s.tracedRecall(ctx, key, d)
	memories := r["memories"].([]Row)
	if len(memories) == 0 {
		info, ok := r["retrieval"].(Row)
		if !ok {
			info = Row{}
			r["retrieval"] = info
		}
		if info["status"] == nil {
			info["status"] = "no_match"
			if d["purpose"] == "skip" {
				info["status"] = "skipped"
			}
		}
		return r
	}
	codec, e := recallCodec()
	must(e)
	result := []Row{}
	used := 0
	for _, m := range memories {
		candidate := append(result, m)
		n, err := codec.Count(encoded(candidate))
		must(err)
		if int64(n) > budget {
			continue
		}
		result = candidate
		used = n
	}
	info := r["retrieval"].(Row)
	if info["status"] == nil {
		info["status"] = "matched"
	}
	info["budget_tokens_used"] = used
	if len(result) < len(memories) {
		info["budget_limited"] = true
		if len(result) == 0 {
			info["status"] = "budget_exhausted"
		}
	}
	r["memories"] = result
	return r
}
