package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Encoder is the one vector space used for both indexing and retrieval.
type Encoder interface {
	ID() string
	External() bool
	Documents(context.Context, []string) ([][]float32, error)
	Search(context.Context, string) ([]float32, error)
}

func (b *BGE) External() bool { return false }
func (b *BGE) ID() string     { return ModelID }
func (b *BGE) Documents(_ context.Context, texts []string) ([][]float32, error) {
	return b.Encode(texts)
}
func (b *BGE) Search(_ context.Context, text string) ([]float32, error) { return b.Query(text) }

const flashModel = "qwen3.7-text-embedding-flash"
const flashDimensions = 1024

var workspaceLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type DashScope struct {
	url    string
	key    string
	client *http.Client
}

func NewDashScope(workspace, key string) (*DashScope, error) {
	if !workspaceLabel.MatchString(workspace) || strings.TrimSpace(key) == "" {
		return nil, errors.New("百炼向量服务需要北京业务空间 ID 和 API Key")
	}
	return &DashScope{
		url: "https://" + workspace + ".cn-beijing.maas.aliyuncs.com/compatible-mode/v1/embeddings",
		key: key, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}
func (d *DashScope) External() bool { return true }
func (d *DashScope) ID() string     { return "dashscope/" + flashModel + "/1024/dense-l2-v1" }

func (d *DashScope) Search(ctx context.Context, text string) ([]float32, error) {
	// Leave time for the authorized lexical fallback on an interactive request.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	vectors, err := d.Documents(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (d *DashScope) Documents(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{"model": flashModel, "input": texts, "dimensions": flashDimensions, "encoding_format": "float"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("百炼向量服务 HTTP %d", resp.StatusCode)
	}
	var output struct {
		Data []struct {
			Index     *int
			Embedding []float32
		}
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&output); err != nil {
		return nil, errors.New("百炼向量响应格式错误")
	}
	if len(output.Data) != len(texts) {
		return nil, errors.New("百炼向量数量与输入不符")
	}
	vectors := make([][]float32, len(texts))
	for _, row := range output.Data {
		if row.Index == nil || *row.Index < 0 || *row.Index >= len(texts) || vectors[*row.Index] != nil || len(row.Embedding) != flashDimensions {
			return nil, errors.New("百炼向量序号或维度错误")
		}
		var sum float64
		for _, v := range row.Embedding {
			sum += float64(v) * float64(v)
		}
		if sum == 0 || math.IsInf(sum, 0) || math.IsNaN(sum) {
			return nil, errors.New("百炼向量不可归一化")
		}
		norm := math.Sqrt(sum)
		for i, v := range row.Embedding {
			row.Embedding[i] = float32(float64(v) / norm)
		}
		vectors[*row.Index] = row.Embedding
	}
	return vectors, nil
}
