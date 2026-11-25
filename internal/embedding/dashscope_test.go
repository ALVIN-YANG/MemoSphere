package embedding

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDashScopeKeepsInputOrderAndVectorSpace(t *testing.T) {
	d, err := NewDashScope("ws-example", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	vec := func(i int) []float32 { v := make([]float32, flashDimensions); v[i] = 5; return v }
	rows := []map[string]any{{"index": 1, "embedding": vec(1)}, {"index": 0, "embedding": vec(0)}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model      string
			Input      []string
			Dimensions int
		}
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-key" || json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != flashModel || request.Dimensions != flashDimensions || len(request.Input) != 2 {
			t.Error("unexpected embedding request")
		}
		json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
	defer server.Close()
	d.url = server.URL
	got, err := d.Documents(t.Context(), []string{"靠窗", "靠过道"})
	if err != nil || len(got) != 2 || got[0][0] != 1 || got[1][1] != 1 {
		t.Fatalf("wrong order or norm: %v", err)
	}
	rows[0]["index"] = 0
	if _, err = d.Documents(t.Context(), []string{"甲", "乙"}); err == nil {
		t.Fatal("accepted duplicate indices")
	}
}
