// Package memory implements the SQLite-backed personal memory service.
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Row map[string]any

type problem struct {
	Status  int
	Message string
}

func (p problem) Error() string       { return p.Message }
func fail(status int, message string) { panic(problem{status, message}) }
func check(ok bool, message string) {
	if !ok {
		fail(400, message)
	}
}
func allowed(ok bool) {
	if !ok {
		fail(403, "无权访问")
	}
}
func required(row Row) Row {
	if row == nil {
		fail(404, "内容不存在或无权访问")
	}
	return row
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func now() float64           { return float64(time.Now().UnixMicro()) / 1e6 }
func digest(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func token(n int) string {
	p := make([]byte, n)
	_, err := rand.Read(p)
	must(err)
	return base64.RawURLEncoding.EncodeToString(p)
}
func str(v any) string { s, _ := v.(string); return s }
func num(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
func sameRevision(value, revision any) bool {
	switch n := value.(type) {
	case int:
		return int64(n) == num(revision)
	case int64:
		return n == num(revision)
	case float64:
		return n == float64(num(revision))
	default:
		return false
	}
}
func truth(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return num(v) != 0
}
func flag(v any) int {
	if truth(v) {
		return 1
	}
	return 0
}
func val(r Row, k string, d any) any {
	if v, ok := r[k]; ok {
		return v
	}
	return d
}
func size(s string) int { return len([]rune(s)) }
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func encoded(v any) string {
	var b strings.Builder
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	must(e.Encode(v))
	return literalSeparators(strings.TrimSuffix(b.String(), "\n"))
}
func parseJSON(s string) any { var v any; must(json.Unmarshal([]byte(s), &v)); return v }
func texts(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		a := make([]string, 0, len(x))
		for _, i := range x {
			s, ok := i.(string)
			check(ok, "需要字符串列表")
			a = append(a, s)
		}
		return a
	case nil:
		return []string{}
	}
	fail(400, "需要字符串列表")
	return nil
}
func in(s string, a []string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func clone(r Row) Row {
	m := Row{}
	for k, v := range r {
		m[k] = v
	}
	return m
}
func pick(r Row, keys ...string) Row {
	m := Row{}
	for _, k := range keys {
		m[k] = r[k]
	}
	return m
}

type DB struct{ *sql.DB }
type tx struct {
	ctx  context.Context
	conn *sql.Conn
}

func (q tx) exec(query string, args ...any) sql.Result {
	res, e := q.conn.ExecContext(q.ctx, query, args...)
	must(e)
	return res
}
func (q tx) rows(query string, args ...any) []Row {
	rows, e := q.conn.QueryContext(q.ctx, query, args...)
	must(e)
	defer rows.Close()
	names, e := rows.Columns()
	must(e)
	result := []Row{}
	for rows.Next() {
		values := make([]any, len(names))
		ptr := make([]any, len(names))
		for i := range ptr {
			ptr[i] = &values[i]
		}
		must(rows.Scan(ptr...))
		m := Row{}
		for i, k := range names {
			m[k] = values[i]
		}
		result = append(result, m)
	}
	must(rows.Err())
	return result
}
func (q tx) one(query string, args ...any) Row {
	rows := q.rows(query, args...)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}
func (q tx) count(query string, args ...any) int64 {
	var n int64
	must(q.conn.QueryRowContext(q.ctx, query, args...).Scan(&n))
	return n
}
func (db DB) transaction(ctx context.Context, write bool, fn func(tx) any) any {
	c, e := db.Conn(ctx)
	must(e)
	defer c.Close()
	begin := "BEGIN"
	if write {
		begin = "BEGIN IMMEDIATE"
	}
	_, e = c.ExecContext(ctx, begin)
	must(e)
	defer c.ExecContext(context.Background(), "ROLLBACK")
	value := fn(tx{ctx, c})
	_, e = c.ExecContext(ctx, "COMMIT")
	must(e)
	return value
}
func (q tx) event(user, action, subject string, details Row) {
	if details == nil {
		details = Row{}
	}
	q.exec("INSERT INTO events(user_id,action,subject,details,created_at) VALUES(?,?,?,?,?)", user, action, subject, encoded(details), now())
}
func (q tx) bump(user any) {
	q.exec("INSERT INTO memory_epochs VALUES(?,1) ON CONFLICT(user_id) DO UPDATE SET epoch=epoch+1", user)
}
func catch(e *error) {
	if p := recover(); p != nil {
		if err, ok := p.(error); ok {
			*e = err
		} else {
			panic(p)
		}
	}
}

//go:embed tables.sql
var tables string

//go:embed indexes.sql
var indexes string

func Open(path string) (result DB, err error) {
	defer catch(&err)
	path, err = filepath.Abs(path)
	if err != nil {
		return
	}
	must(os.MkdirAll(filepath.Dir(path), 0700))
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	must(e)
	must(f.Chmod(0600))
	must(f.Close())
	u := url.URL{Scheme: "file", Path: path}
	p := url.Values{}
	p.Add("_pragma", "busy_timeout(5000)")
	p.Add("_pragma", "temp_store(MEMORY)")
	u.RawQuery = p.Encode()
	db, e := sql.Open("sqlite", u.String())
	must(e)
	db.SetMaxOpenConns(8)
	ready := false
	defer func() {
		if !ready {
			db.Close()
		}
	}()
	_, e = db.Exec("PRAGMA journal_mode=WAL")
	must(e)
	result = DB{db}
	result.transaction(context.Background(), true, func(q tx) any {
		q.exec(tables)
		// Read the desired column definitions from the versioned schema. Older
		// SQLite files may predate authorization, evidence or vector indexing.
		for table, columns := range legacyColumns {
			existing := map[string]bool{}
			for _, col := range q.rows("PRAGMA table_info(" + table + ")") {
				existing[str(col["name"])] = true
			}
			for _, col := range columns {
				name := strings.Fields(col)[0]
				if !existing[name] {
					q.exec("ALTER TABLE " + table + " ADD COLUMN " + col)
					if name == "basis" {
						q.exec("UPDATE memories SET basis='user_statement' WHERE origin='manual'")
					}
				}
			}
		}
		q.exec(indexes)
		migrateCategories(q)
		q.exec("UPDATE memory_versions SET disposition='superseded' WHERE disposition='current' AND revision<(SELECT revision FROM memories WHERE id=memory_id)")
		q.exec("INSERT OR IGNORE INTO extraction_jobs(message_id,state,error,updated_at) SELECT id,'skipped','legacy_archive',? FROM messages", now())
		for _, r := range q.rows("SELECT m.*,j.error FROM messages m LEFT JOIN extraction_jobs j ON j.message_id=m.id WHERE m.stream_id IS NULL") {
			attach(q, r, r["error"] != "legacy_archive")
		}
		return nil
	})
	ready = true
	return result, nil
}

var legacyColumns = map[string][]string{
	"agents":          {"read_access INTEGER NOT NULL DEFAULT 1", "write_access INTEGER NOT NULL DEFAULT 1"},
	"messages":        {"spoken_at TEXT", "stream_id TEXT", "content_state TEXT NOT NULL DEFAULT 'raw'", "content_revision INTEGER NOT NULL DEFAULT 1", "content_hash TEXT", "raw_bytes INTEGER NOT NULL DEFAULT 0", "managed INTEGER NOT NULL DEFAULT 1"},
	"memories":        {"state TEXT NOT NULL DEFAULT 'active'", "origin TEXT NOT NULL DEFAULT 'manual'", "kind TEXT NOT NULL DEFAULT 'profile'", "last_operation TEXT NOT NULL DEFAULT 'ADD'", "basis TEXT NOT NULL DEFAULT 'unverified'", "legacy_category TEXT"},
	"memory_versions": {"disposition TEXT NOT NULL DEFAULT 'current'", "operation TEXT NOT NULL DEFAULT 'USER_EDIT'"},
	"model_usage":     {"outcome TEXT NOT NULL DEFAULT 'unknown'", "purpose TEXT NOT NULL DEFAULT 'extraction'"},
	"memory_sources":  {"source_start INTEGER", "source_end INTEGER"},
}

func internalError(p any) problem {
	if v, ok := p.(problem); ok {
		return v
	}
	if errors.Is(asError(p), context.Canceled) {
		return problem{408, "请求已取消"}
	}
	return problem{500, "记忆服务暂时不可用"}
}
func asError(p any) error {
	if err, ok := p.(error); ok {
		return err
	}
	return fmt.Errorf("internal failure")
}
