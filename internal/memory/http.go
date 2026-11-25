package memory

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func bearer(r *http.Request) string {
	p := strings.Fields(r.Header.Get("Authorization"))
	if len(p) == 2 && strings.EqualFold(p[0], "bearer") {
		return p[1]
	}
	return ""
}
func body(w http.ResponseWriter, r *http.Request) Row {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if e != nil {
		fail(413, "提交内容过大")
	}
	var d Row
	check(json.Unmarshal(b, &d) == nil && d != nil, "提交的数据格式无效")
	return d
}
func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Service) rate(r *http.Request, scope string, limit int) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	k := scope + "/" + host
	n := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := func(a []time.Time) []time.Time {
		for len(a) > 0 && n.Sub(a[0]) >= time.Minute {
			a = a[1:]
		}
		return a
	}
	a := cutoff(s.attempts[k])
	if len(a) >= limit {
		fail(429, "操作太频繁，请稍后重试")
	}
	s.attempts[k] = append(a, n)
	if len(s.attempts) > 10000 {
		for key, list := range s.attempts {
			if len(cutoff(list)) == 0 {
				delete(s.attempts, key)
			}
		}
	}
}
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { send(w, 200, Row{"status": "ok", "version": "0.3.0"}) })
	if s.Assets != nil {
		files := http.FileServer(http.FS(s.Assets))
		mux.Handle("GET /assets/", http.StripPrefix("/assets", files))
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { files.ServeHTTP(w, r) })
	}
	mux.HandleFunc("POST /api/auth/code", func(w http.ResponseWriter, r *http.Request) {
		s.origin(r)
		s.rate(r, "mail", 10)
		send(w, 200, s.sendCode(r.Context(), body(w, r)))
	})
	for _, purpose := range []string{"login", "register"} {
		mux.HandleFunc("POST /api/auth/"+purpose, func(w http.ResponseWriter, r *http.Request) {
			s.origin(r)
			s.rate(r, "auth", 20)
			v, key := s.authenticate(r.Context(), body(w, r), purpose)
			s.cookie(w, key, 7*86400)
			send(w, 200, v)
		})
	}
	mux.HandleFunc("POST /api/connect/exchange", func(w http.ResponseWriter, r *http.Request) {
		s.rate(r, "pairing", 30)
		d := body(w, r)
		code := str(d["code"])
		check(len(code) <= 128, "接入码格式无效")
		v := s.DB.transaction(r.Context(), true, func(q tx) any { return s.exchange(q, code) })
		send(w, 200, v)
	})
	mux.HandleFunc("GET /api/connect/policy", func(w http.ResponseWriter, r *http.Request) {
		v := s.DB.transaction(r.Context(), false, func(q tx) any {
			a := agent(q, bearer(r))
			res := permissions(a)
			res["memory_protocol"] = "retain-recall"
			return res
		})
		send(w, 200, v)
	})
	for _, route := range []string{
		"GET /api/session",
		"POST /api/auth/logout",
		"POST /api/hosted/grant",
		"GET /api/projects",
		"POST /api/projects",
		"GET /api/memories",
		"POST /api/memories",
		"GET /api/memories/{id}",
		"PUT /api/memories/{id}",
		"DELETE /api/memories/{id}",
		"GET /api/agents",
		"POST /api/agents",
		"PATCH /api/agents/{id}",
		"DELETE /api/agents/{id}",
		"POST /api/agents/{id}/setup",
		"POST /api/agents/{id}/rotate",
		"GET /api/activity",
		"GET /api/conversations",
		"GET /api/export",
	} {
		mux.HandleFunc(route, s.ownerRequest)
	}
	mux.HandleFunc("GET /v1/operations/{operation}", func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, s.operation(r.Context(), bearer(r), r.PathValue("operation")))
	})
	mux.Handle("/mcp/", s.mcpHandler())
	host, _ := url.Parse(s.Origin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		defer func() {
			if p := recover(); p != nil {
				if c, ok := p.(cooldown); ok {
					w.Header().Set("Retry-After", strconv.Itoa(c.seconds))
					send(w, 429, Row{"error": "请稍后重新发送", "resend_after": c.seconds})
					return
				}
				e := internalError(p)
				if e.Status == 500 {
					log.Printf("request failed: %T", p)
				}
				send(w, e.Status, Row{"error": e.Message})
			}
		}()
		if r.Host != host.Host {
			fail(400, "服务地址不匹配")
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Service) origin(r *http.Request) {
	if r.Header.Get("Origin") != s.Origin {
		fail(403, "请求来源不符合当前服务地址")
	}
}
func (s *Service) cookie(w http.ResponseWriter, key string, age int) {
	http.SetCookie(w, &http.Cookie{Name: "ms_session", Value: key, Path: "/", MaxAge: age, Secure: strings.HasPrefix(s.Origin, "https://"), HttpOnly: true, SameSite: http.SameSiteLaxMode})
}
func (s *Service) ownerRequest(w http.ResponseWriter, r *http.Request) {
	write := r.Method != "GET"
	c, e := r.Cookie("ms_session")
	if e != nil {
		fail(401, "请先登录")
	}
	id := r.PathValue("id")
	status := 200
	reply := s.DB.transaction(r.Context(), write, func(q tx) any {
		user := q.one("SELECT u.id,u.email,t.csrf FROM sessions t JOIN users u ON u.id=t.user_id WHERE t.token_hash=? AND t.expires_at>?", digest(c.Value), now())
		if user == nil {
			fail(401, "请先登录")
		}
		uid := str(user["id"])
		if write {
			s.origin(r)
			allowed(hmac.Equal([]byte(str(user["csrf"])), []byte(r.Header.Get("X-CSRF-Token"))))
		}
		switch r.Pattern {
		case "GET /api/session":
			return s.session(q, user)
		case "POST /api/auth/logout":
			q.exec("DELETE FROM sessions WHERE token_hash=?", digest(c.Value))
		case "POST /api/hosted/grant":
			d := body(w, r)
			check(len(d) == 1 && d["consent"] == true, "请确认允许 KoKo 读写专属项目")
			return hosted(q, uid, r.Header.Get("X-Hosted-Client"))
		case "GET /api/projects":
			return Row{"items": q.rows("SELECT * FROM projects WHERE user_id=? ORDER BY created_at", uid)}
		case "POST /api/projects":
			status = 201
			return project(q, uid, str(body(w, r)["name"]))
		case "GET /api/memories":
			return library(q, uid, r.URL.Query())
		case "POST /api/memories":
			status = 201
			return save(q, uid, "", body(w, r))
		case "GET /api/memories/{id}":
			return detail(q, uid, id)
		case "PUT /api/memories/{id}":
			return save(q, uid, id, body(w, r))
		case "DELETE /api/memories/{id}":
			return Row{"ok": true, "deleted": erase(q, uid, id)}
		case "GET /api/agents":
			return Row{"items": listAgents(q, uid)}
		case "POST /api/agents":
			status = 201
			return createAgent(q, uid, body(w, r))
		case "PATCH /api/agents/{id}":
			updateAgent(q, uid, id, body(w, r))
		case "DELETE /api/agents/{id}":
			revoke(q, uid, id)
		case "POST /api/agents/{id}/setup":
			return s.setup(q, uid, id)
		case "POST /api/agents/{id}/rotate":
			a := owned(q, uid, id)
			check(a["revoked_at"] == nil, "Agent 已撤销")
			key, _ := replaceKey(q, a)
			q.event(uid, "agent.key_rotated", id, nil)
			return Row{"key": key}
		case "GET /api/activity":
			rows := q.rows("SELECT id,action,subject,details,created_at FROM events WHERE user_id=? ORDER BY id DESC LIMIT 100", uid)
			for _, r := range rows {
				r["details"] = parseJSON(str(r["details"]))
			}
			return Row{"items": rows}
		case "GET /api/conversations":
			return s.conversations(q, uid)
		case "GET /api/export":
			return export(q, uid)
		}
		return Row{"ok": true}
	})
	if r.Pattern == "POST /api/auth/logout" {
		s.cookie(w, "", -1)
	}
	if r.Pattern == "GET /api/export" {
		w.Header().Set("Content-Disposition", `attachment; filename="memosphere-memories.json"`)
	}
	send(w, status, reply)
}
func (s *Service) session(q tx, user Row) Row {
	uid := user["id"]
	counts := Row{}
	for name, table := range map[string]string{"memories": "memories", "projects": "projects", "agents": "connections", "messages": "messages"} {
		query := "SELECT count(*) FROM " + table + " WHERE user_id=?"
		if name == "agents" {
			query += " AND revoked_at IS NULL"
		}
		counts[name] = q.count(query, uid)
	}
	states := Row{}
	for _, r := range q.rows("SELECT j.state,count(*) n FROM extraction_jobs j JOIN messages m ON m.id=j.message_id WHERE m.user_id=? GROUP BY j.state", uid) {
		states[str(r["state"])] = r["n"]
	}
	return Row{"user": pick(user, "id", "email"), "csrf": user["csrf"], "counts": counts, "public_url": s.Origin, "automatic_extraction": s.Model != nil, "processing": states, "model_usage": q.one("SELECT count(*) calls,coalesce(sum(input_tokens),0) input_tokens,coalesce(sum(output_tokens),0) output_tokens FROM model_usage u JOIN messages m ON m.id=u.message_id WHERE m.user_id=?", uid), "memory_epoch": q.count("SELECT coalesce(max(epoch),0) FROM memory_epochs WHERE user_id=?", uid), "conversation_storage": s.storage(q, str(uid))}
}
func export(q tx, uid string) Row {
	items := []Row{}
	for _, r := range q.rows("SELECT * FROM memories WHERE user_id=? ORDER BY id", uid) {
		i := decodeMemory(r)
		v := q.rows("SELECT * FROM memory_versions WHERE memory_id=? ORDER BY revision DESC", r["id"])
		for _, x := range v {
			x["content"] = parseJSON(str(x["content"]))
		}
		i["versions"] = v
		i["sources"] = q.rows("SELECT revision,message_id,quote FROM memory_sources WHERE memory_id=?", r["id"])
		items = append(items, i)
	}
	summaries := q.rows("SELECT * FROM conversation_streams WHERE user_id=? AND covered_messages>0", uid)
	for _, summary := range summaries {
		summary["summary"] = parseJSON(str(summary["summary"]))
	}
	return Row{"format": "memosphere-v1", "exported_at": now(), "projects": q.rows("SELECT * FROM projects WHERE user_id=?", uid), "memories": items, "conversations": q.rows("SELECT * FROM messages WHERE user_id=? ORDER BY id", uid), "conversation_summaries": summaries}
}
