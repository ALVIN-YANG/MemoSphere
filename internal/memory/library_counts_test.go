package memory

import (
	"net/url"
	"path/filepath"
	"testing"
)

func TestLibraryCountsCoverTheOwnersWholeLibrary(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "owner.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.transaction(t.Context(), true, func(q tx) any {
		for range 51 {
			save(q, "a", "", Row{"text": "行程", "category": "事件"})
		}
		save(q, "b", "", Row{"text": "另一个账号", "category": "规则"})
		parent := save(q, "a", "", Row{"text": "靠窗", "category": "偏好"})
		save(q, "a", "", Row{"text": "可能喜欢靠窗", "category": "偏好", "inputs": []Row{{"id": parent["id"], "revision": 1}}})
		page := library(q, "a", url.Values{"order": {"created"}, "category": {"事件"}, "offset": {"50"}})
		counts := page["counts"].(Row)
		states := counts["states"].(map[string]int64)
		cats := counts["categories"].(map[string]int64)
		if len(page["items"].([]Row)) != 1 || counts["all"] != int64(53) ||
			states["uncertain"] != 1 || states["active"] != 52 || len(cats) != 2 || cats["事件"] != 51 {
			t.Errorf("unexpected counts on the filtered last page: %v", counts)
		}
		return nil
	})
}
