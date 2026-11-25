// The application API is for one trusted host application per private instance.
// Its subjects are the host's stable user IDs, independent of console identities.
package memory

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strings"
)

const appSpacesDDL = `CREATE TABLE IF NOT EXISTS application_spaces(
 subject TEXT PRIMARY KEY, owner TEXT NOT NULL UNIQUE, project TEXT NOT NULL UNIQUE,
 connection TEXT NOT NULL UNIQUE, agent_key TEXT NOT NULL)`

// ApplicationHandler exposes only the application contract. No owner cookie or
// user-facing registration endpoint is mounted on this listener.
func (s *Service) ApplicationHandler(secret string) (http.Handler, error) {
	if len(secret) < 32 || strings.TrimSpace(secret) != secret {
		return nil, problem{400, "应用服务密钥至少需要 32 个字符"}
	}
	if err := s.DB.PrepareApplicationDB(context.Background()); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { send(w, 200, Row{"status": "ok", "mode": "application"}) })
	for _, route := range []string{"GET /v1/app/projects", "GET /v1/app/memories", "POST /v1/app/memories", "GET /v1/app/memories/{id}", "PUT /v1/app/memories/{id}", "DELETE /v1/app/memories/{id}", "POST /v1/app/retain", "POST /v1/app/recall", "GET /v1/app/operations/{operation}"} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(secret)) != 1 {
				fail(401, "应用身份无效")
			}
			subject := r.Header.Get("X-Memory-Subject")
			check(subject != "" && len(subject) <= 200 && subject == strings.TrimSpace(subject), "缺少有效的应用用户标识")
			s.applicationRequest(w, r, subject)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		defer func() {
			if p := recover(); p != nil {
				e := internalError(p)
				send(w, e.Status, Row{"error": e.Message})
			}
		}()
		mux.ServeHTTP(w, r)
	}), nil
}
func appSpace(q tx, subject, prior string) Row {
	space := q.one("SELECT * FROM application_spaces WHERE subject=?", subject)
	if prior != "" && (space == nil || space["project"] != prior) {
		fail(409, "该账号的既有记忆项目尚未迁入此实例")
	}
	if space != nil {
		return space
	}
	owner := uuid.NewString()
	projectID := str(project(q, owner, "应用记忆")["id"])
	connection := uuid.NewString()
	key := "internal_" + token(32)
	insertAppSpace(q, subject, owner, projectID, connection, key)
	return q.one("SELECT * FROM application_spaces WHERE subject=?", subject)
}
func insertAppSpace(q tx, subject, owner, projectID, connection, key string) {
	hash := digest(key)
	q.exec("INSERT INTO application_spaces VALUES(?,?,?,?,?)", subject, owner, projectID, connection, key)
	q.exec("INSERT INTO agents(token_hash,agent_id,user_id,projects,global_access,sensitive_access,revoked,read_access,write_access) VALUES(?,?,?,?,1,0,0,1,1)", hash, connection, owner, encoded([]string{projectID}))
	q.exec("INSERT INTO connections(id,user_id,name,kind,token_hash,created_at) VALUES(?,?,?,?,?,?)", connection, owner, "应用助理", "other", hash, now())
}
func (s *Service) applicationRequest(w http.ResponseWriter, r *http.Request, subject string) {
	ctx := r.Context()
	prior := r.Header.Get("X-Memory-Prior-Project")
	// Existing subjects do not need a writer lock for a page read or vector query.
	space := s.DB.transaction(ctx, false, func(q tx) any { return q.one("SELECT * FROM application_spaces WHERE subject=?", subject) }).(Row)
	if space == nil {
		space = s.DB.transaction(ctx, true, func(q tx) any { return appSpace(q, subject, prior) }).(Row)
	} else if prior != "" && space["project"] != prior {
		fail(409, "原记忆项目不匹配")
	}
	owner, key := str(space["owner"]), str(space["agent_key"])
	if r.Pattern == "GET /v1/app/operations/{operation}" {
		send(w, 200, s.operation(ctx, key, r.PathValue("operation")))
		return
	}
	var data Row
	if r.Method == "PUT" || r.Method == "POST" {
		limit := int64(65536)
		if r.Pattern == "POST /v1/app/retain" {
			limit = 524288
		}
		raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if e != nil {
			fail(413, "提交内容过大")
		}
		check(json.Unmarshal(raw, &data) == nil && data != nil, "提交的数据格式无效")
		for _, field := range []string{"user_id", "subject", "owner", "account_id", "connection_id"} {
			if _, found := data[field]; found {
				fail(400, "身份由应用服务端提供")
			}
		}
	}
	if r.Pattern == "POST /v1/app/recall" || r.Pattern == "POST /v1/app/retain" {
		if _, ok := data["project_id"]; ok {
			fail(400, "会话项目由服务端确定")
		}
		data["project_id"] = space["project"]
		if r.Pattern == "POST /v1/app/recall" {
			traced := context.WithValue(ctx, parentTrace{}, r.Header.Get("traceparent"))
			send(w, 200, s.recallForClient(traced, key, data))
			return
		}
		send(w, 202, s.retain(ctx, key, data))
		return
	}
	status := 200
	id := r.PathValue("id")
	result := s.DB.transaction(ctx, r.Method != "GET", func(q tx) any {
		switch r.Pattern {
		case "GET /v1/app/projects":
			return Row{"items": q.rows("SELECT id,name FROM projects WHERE user_id=? ORDER BY created_at", owner)}
		case "GET /v1/app/memories":
			return library(q, owner, r.URL.Query())
		case "GET /v1/app/memories/{id}":
			return detail(q, owner, id)
		case "POST /v1/app/memories":
			status = 201
			return save(q, owner, "", data)
		case "PUT /v1/app/memories/{id}":
			return save(q, owner, id, data)
		case "DELETE /v1/app/memories/{id}":
			return Row{"ok": true, "deleted": erase(q, owner, id)}
		}
		fail(404, "接口不存在")
		return nil
	})
	send(w, status, result)
}
