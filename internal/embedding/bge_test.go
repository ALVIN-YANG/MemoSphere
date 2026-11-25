package embedding

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestPythonReference(t *testing.T) {
	path := os.Getenv("MEMOSPHERE_EMBEDDING_ORACLE")
	if path == "" {
		t.Skip("set oracle path to compare the reference model")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Texts   []string
		IDs     [][]int64
		Vectors [][]float32
	}
	if err = json.Unmarshal(data, &ref); err != nil {
		t.Fatal(err)
	}
	model, err := Open(os.Getenv("MEMOSPHERE_EMBEDDING_DIR"), os.Getenv("MEMOSPHERE_ONNX_LIBRARY"))
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	vecs, err := model.Encode(ref.Texts)
	if err != nil {
		t.Fatal(err)
	}
	var delta float64
	for i, s := range ref.Texts {
		ids := model.IDs(s)
		if len(ids) != len(ref.IDs[i]) {
			t.Fatalf("token count for input %d", i)
		}
		for j, id := range ids {
			if ref.IDs[i][j] != id {
				t.Fatalf("input %d token %d mismatch", i, j)
			}
		}
		for j, v := range vecs[i] {
			delta = max(delta, math.Abs(float64(v-ref.Vectors[i][j])))
		}
	}
	if delta > 2e-5 {
		t.Fatalf("max coordinate delta %g", delta)
	}
	t.Logf("%d inputs, 512 dimensions, max absolute error %g", len(ref.Texts), delta)
}
