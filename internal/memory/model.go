package memory

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

//go:embed prompts/extract.txt
var extractionPrompt string

//go:embed prompts/admit.txt
var admissionPrompt string

//go:embed prompts/summarize.txt
var summaryPrompt string

//go:embed prompts/verify.txt
var verificationPrompt string

type Model struct {
	BaseURL   string  `json:"base_url"`
	Name      string  `json:"model"`
	Key       string  `json:"api_key"`
	Thinking  string  `json:"thinking"`
	MaxTokens int     `json:"max_tokens"`
	Timeout   float64 `json:"timeout"`
	client    *http.Client
}

func LoadModel(path string) (*Model, error) {
	m := &Model{Timeout: 90, Thinking: "disabled", MaxTokens: 6144}
	if e := privateJSON(path, m); e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return nil, nil
		}
		return nil, e
	}
	u, e := url.Parse(m.BaseURL)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) || m.Name == "" || m.Key == "" || !in(m.Thinking, []string{"enabled", "disabled"}) || m.Timeout <= 0 || m.MaxTokens <= 0 {
		return nil, errors.New("invalid model configuration")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	m.client = &http.Client{Transport: transport, Timeout: time.Duration(m.Timeout * float64(time.Second)), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return m, nil
}

type jobError string

func (j jobError) Error() string { return string(j) }
func ensure(ok bool, code jobError) {
	if !ok {
		panic(code)
	}
}
func failure(p any) string {
	if e, ok := p.(jobError); ok {
		return string(e)
	}
	if e, ok := p.(problem); ok && e.Status == 403 {
		return "authorization_changed"
	}
	return "processing_error"
}

type tokenUsage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
	Details    struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (m *Model) complete(ctx context.Context, prompt string, payload Row, usage *tokenUsage) Row {
	body := Row{"model": m.Name, "response_format": Row{"type": "json_object"}, "messages": []Row{{"role": "system", "content": prompt}, {"role": "user", "content": encoded(payload)}}, "max_tokens": m.MaxTokens, "thinking": Row{"type": m.Thinking}, "stream": false}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(m.BaseURL, "/")+"/chat/completions", strings.NewReader(encoded(body)))
	must(e)
	req.Header.Set("Authorization", "Bearer "+m.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, e := m.client.Do(req)
	ensure(e == nil, "model_unreachable")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		panic(jobError(fmt.Sprintf("model_http_%d", resp.StatusCode)))
	}
	var reply struct {
		Usage   tokenUsage
		Choices []struct {
			Finish  string `json:"finish_reason"`
			Message struct{ Content string }
		}
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	ensure(e == nil && json.Unmarshal(raw, &reply) == nil && len(reply.Choices) > 0, "model_invalid_json")
	*usage = reply.Usage
	c := reply.Choices[0]
	ensure(c.Finish == "stop", "model_incomplete")
	var answer Row
	ensure(size(c.Message.Content) <= 100000 && json.Unmarshal([]byte(c.Message.Content), &answer) == nil && answer != nil, "model_invalid_json")
	return answer
}
func (s *Service) call(ctx context.Context, id any, purpose, prompt string, payload Row) (Row, int64) {
	started := time.Now()
	var usage tokenUsage
	db := s.DB
	res, e := db.ExecContext(ctx, "INSERT INTO model_usage(message_id,model,input_tokens,output_tokens,reasoning_tokens,latency,created_at,outcome,purpose) VALUES(?,?,0,0,0,0,?,'requested',?)", id, s.Model.Name, now(), purpose)
	must(e)
	uid, e := res.LastInsertId()
	must(e)
	defer func() {
		p := recover()
		outcome := "returned"
		if p != nil {
			outcome = failure(p)
		}
		_, e := db.Exec("UPDATE model_usage SET input_tokens=?,output_tokens=?,reasoning_tokens=?,latency=?,outcome=? WHERE id=?", max(0, usage.Prompt), max(0, usage.Completion), max(0, usage.Details.Reasoning), time.Since(started).Seconds(), outcome, uid)
		must(e)
		if p != nil {
			panic(p)
		}
	}()
	return s.Model.complete(ctx, prompt, payload, &usage), uid
}
func (s *Service) outcome(id int64, status string) {
	if id > 0 {
		_, e := s.DB.Exec("UPDATE model_usage SET outcome=? WHERE id=?", status, id)
		must(e)
	}
}
func strict(raw Row, to any, code jobError) {
	d := json.NewDecoder(bytes.NewBufferString(encoded(raw)))
	d.DisallowUnknownFields()
	ensure(d.Decode(to) == nil, code)
}
func truncated(s string, n int) string { r := []rune(s); return string(r[:min(n, len(r))]) }
