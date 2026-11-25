// Package embedding runs the same pinned BGE WordPiece tokenizer and ONNX model
// used to build MemoSphere's existing 512-dimensional SQLite vectors.
package embedding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	ort "github.com/yalue/onnxruntime_go"
)

const ModelID = "bge-small-zh-v1.5/75c43b06/int8-cls-l2-v1"
const prefix = "为这个句子生成表示以用于检索相关文章："

var strip = regexp.MustCompile(`^\s*(?:我(?:以前|之前|先前|曾经)?(?:说过|提过)|还记得我(?:说过|提过))[，,：:、]\s*`)
var special = regexp.MustCompile(`\[(?:PAD|UNK|CLS|SEP|MASK)\]`)

type BGE struct {
	vocab   map[string]int64
	session *ort.DynamicAdvancedSession
}

func Open(dir, library string) (*BGE, error) {
	for name, want := range map[string]string{"model.onnx": "15b717c382bcb518ba457b93ea6850ede7f4f1cd8937454aa06972366cd19bcc", "tokenizer.json": "48cea5d44424912a6fd1ea647bf4fe50b55ab8b1e5879c3275f80e339e8fae26"} {
		f, e := os.Open(filepath.Join(dir, name))
		if e != nil {
			return nil, e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return nil, e
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return nil, fmt.Errorf("invalid digest for %s", name)
		}
	}
	data, e := os.ReadFile(filepath.Join(dir, "tokenizer.json"))
	if e != nil {
		return nil, e
	}
	var t struct {
		Model struct{ Vocab map[string]int64 }
	}
	if e = json.Unmarshal(data, &t); e != nil {
		return nil, e
	}
	if !ort.IsInitialized() {
		if library != "" {
			ort.SetSharedLibraryPath(library)
		}
		if e = ort.InitializeEnvironment(); e != nil {
			return nil, e
		}
	}
	opts, e := ort.NewSessionOptions()
	if e != nil {
		return nil, e
	}
	defer opts.Destroy()
	if e = opts.SetIntraOpNumThreads(2); e != nil {
		return nil, e
	}
	if e = opts.SetInterOpNumThreads(1); e != nil {
		return nil, e
	}
	session, e := ort.NewDynamicAdvancedSession(filepath.Join(dir, "model.onnx"), []string{"input_ids", "attention_mask", "token_type_ids"}, []string{"last_hidden_state"}, opts)
	if e != nil {
		return nil, e
	}
	return &BGE{t.Model.Vocab, session}, nil
}
func (b *BGE) Close() { b.session.Destroy() }
func cjk(c rune) bool {
	return c >= 0x4e00 && c <= 0x9fff || c >= 0x3400 && c <= 0x4dbf || c >= 0x20000 && c <= 0x2a6df || c >= 0x2a700 && c <= 0x2b73f || c >= 0x2b740 && c <= 0x2b81f || c >= 0x2b820 && c <= 0x2ceaf || c >= 0xf900 && c <= 0xfaff || c >= 0x2f800 && c <= 0x2fa1f
}
func (b *BGE) IDs(text string) []int64 {
	ids := []int64{101}
	word := []rune{}
	flush := func() {
		if len(word) == 0 {
			return
		}
		piece := []int64{}
		for i := 0; i < len(word); {
			end := len(word)
			var id int64
			ok := false
			if len(word) <= 100 {
				for ; end > i; end-- {
					key := string(word[i:end])
					if i > 0 {
						key = "##" + key
					}
					if id, ok = b.vocab[key]; ok {
						break
					}
				}
			}
			if !ok {
				piece = []int64{100}
				break
			}
			piece = append(piece, id)
			i = end
		}
		ids = append(ids, piece...)
		word = word[:0]
	}
	split := func(s string) {
		for _, r := range s {
			switch {
			case r == '\t' || r == '\n' || r == '\r' || unicode.Is(unicode.Zs, r):
				flush()
			case r == 0 || r == 0xfffd || unicode.In(r, unicode.Cc, unicode.Cf):
				continue
			case cjk(r) || unicode.IsPunct(r) || (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126):
				flush()
				word = append(word, r)
				flush()
			default:
				word = append(word, r)
			}
		}
		flush()
	}
	start := 0
	for _, span := range special.FindAllStringIndex(text, -1) {
		split(text[start:span[0]])
		ids = append(ids, b.vocab[text[span[0]:span[1]]])
		start = span[1]
	}
	split(text[start:])
	if len(ids) > 511 {
		ids = ids[:511]
	}
	return append(ids, 102)
}
func (b *BGE) Encode(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	tokens := make([][]int64, len(texts))
	width := 0
	for i, s := range texts {
		tokens[i] = b.IDs(s)
		width = max(width, len(tokens[i]))
	}
	shape := ort.NewShape(int64(len(texts)), int64(width))
	ids := make([]int64, len(texts)*width)
	mask := make([]int64, len(ids))
	types := make([]int64, len(ids))
	for i, seq := range tokens {
		copy(ids[i*width:], seq)
		for j := range seq {
			mask[i*width+j] = 1
		}
	}
	inputs := []ort.Value{}
	for _, v := range [][]int64{ids, mask, types} {
		t, e := ort.NewTensor(shape, v)
		if e != nil {
			return nil, e
		}
		defer t.Destroy()
		inputs = append(inputs, t)
	}
	out := []ort.Value{nil}
	if e := b.session.Run(inputs, out); e != nil {
		return nil, e
	}
	defer out[0].Destroy()
	tensor, ok := out[0].(*ort.Tensor[float32])
	if !ok || len(tensor.GetData()) != len(ids)*512 {
		return nil, errors.New("unexpected BGE output")
	}
	result := make([][]float32, len(texts))
	for i := range texts {
		v := append([]float32(nil), tensor.GetData()[i*width*512:i*width*512+512]...)
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if !(sum > 0) || math.IsInf(sum, 0) {
			return nil, errors.New("invalid BGE norm")
		}
		norm := float32(math.Sqrt(sum))
		for j := range v {
			v[j] /= norm
		}
		result[i] = v
	}
	return result, nil
}
func (b *BGE) Query(s string) ([]float32, error) {
	x := strings.TrimSpace(strip.ReplaceAllString(s, ""))
	if x == "" {
		x = s
	}
	v, e := b.Encode([]string{prefix + x})
	if e != nil {
		return nil, e
	}
	return v[0], nil
}
