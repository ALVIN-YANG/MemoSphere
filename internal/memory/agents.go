package memory

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"github.com/google/uuid"
	"os"
	"strings"
)

func project(q tx, uid, name string) Row {
	name = strings.TrimSpace(name)
	check(size(name) > 0 && size(name) <= 80, "项目名称需为 1 至 80 个字符")
	id := uuid.NewString()
	q.exec("INSERT INTO projects VALUES(?,?,?,?)", id, uid, name, now())
	q.event(uid, "project.created", id, Row{"name": name})
	return q.one("SELECT * FROM projects WHERE id=?", id)
}
func checkProjects(q tx, uid string, projects []string) {
	check(len(projects) <= 100, "项目列表无效")
	for _, id := range projects {
		allowed(q.one("SELECT id FROM projects WHERE id=? AND user_id=?", id, uid) != nil)
	}
}
func permissions(a Row) Row {
	p := Row{"projects": parseJSON(str(a["projects"]))}
	for _, k := range []string{"global_access", "sensitive_access", "read_access", "write_access"} {
		p[k] = truth(a[k])
	}
	return p
}
func publicAgent(a Row) Row {
	delete(a, "token_hash")
	for k, v := range permissions(a) {
		a[k] = v
	}
	return a
}

const agentJoin = "SELECT c.*,a.projects,a.global_access,a.sensitive_access,a.read_access,a.write_access FROM connections c JOIN agents a ON a.token_hash=c.token_hash "

func listAgents(q tx, uid string) []Row {
	rows := q.rows(agentJoin+"WHERE c.user_id=? ORDER BY c.created_at DESC", uid)
	for _, a := range rows {
		publicAgent(a)
	}
	return rows
}
func createAgent(q tx, uid string, d Row) Row {
	name := strings.TrimSpace(str(d["name"]))
	kind := str(d["kind"])
	projects := texts(d["projects"])
	check(size(name) > 0 && size(name) <= 80 && in(kind, []string{"codex", "kimi", "other"}), "请输入 Agent 名称并选择类型")
	check(len(projects) > 0, "请先选择一个项目")
	checkProjects(q, uid, projects)
	id := uuid.NewString()
	hash := digest("ms_" + token(32))
	q.exec("INSERT INTO agents(token_hash,agent_id,user_id,projects,global_access,sensitive_access,revoked,read_access,write_access) VALUES(?,?,?,?,?,?,0,?,?)", hash, id, uid, encoded(projects), flag(val(d, "global_access", true)), flag(d["sensitive_access"]), flag(val(d, "read_access", true)), flag(val(d, "write_access", true)))
	q.exec("INSERT INTO connections VALUES(?,?,?,?,?,?,NULL,NULL,NULL)", id, uid, name, kind, hash, now())
	q.event(uid, "agent.created", id, Row{"name": name, "kind": kind})
	return publicAgent(q.one(agentJoin+"WHERE c.id=?", id))
}
func owned(q tx, uid, id string) Row {
	return required(q.one("SELECT * FROM connections WHERE id=? AND user_id=?", id, uid))
}
func updateAgent(q tx, uid, id string, d Row) {
	a := owned(q, uid, id)
	check(a["revoked_at"] == nil, "Agent 已撤销")
	projects := texts(d["projects"])
	checkProjects(q, uid, projects)
	check(len(projects) > 0 || truth(d["global_access"]), "至少授权一个项目或个人全局记忆")
	q.exec("UPDATE agents SET projects=?,global_access=?,sensitive_access=?,read_access=?,write_access=? WHERE token_hash=?", encoded(projects), flag(d["global_access"]), flag(d["sensitive_access"]), flag(d["read_access"]), flag(d["write_access"]), a["token_hash"])
	q.event(uid, "agent.permissions_updated", id, nil)
}
func revoke(q tx, uid, id string) {
	a := owned(q, uid, id)
	q.exec("UPDATE agents SET revoked=1 WHERE token_hash=?", a["token_hash"])
	q.exec("UPDATE connections SET revoked_at=? WHERE id=?", now(), id)
	q.exec("DELETE FROM setup_codes WHERE connection_id=?", id)
	q.event(uid, "agent.revoked", id, nil)
}
func (s *Service) setup(q tx, uid, id string) Row {
	a := owned(q, uid, id)
	check(a["revoked_at"] == nil, "Agent 已撤销")
	code := "pair_" + token(24)
	q.exec("DELETE FROM setup_codes WHERE connection_id=? OR expires_at<?", id, now())
	q.exec("INSERT INTO setup_codes VALUES(?,?,?,?,NULL)", digest(code), id, a["token_hash"], now()+600)
	q.event(uid, "agent.setup_created", id, nil)
	r := Row{"code": code, "expires_in": 600}
	if a["kind"] == "other" {
		r["url"] = s.Origin + "/mcp/"
		r["exchange_url"] = s.Origin + "/api/connect/exchange"
	} else {
		r["command"] = "uv run python -m memosphere.connect --url '" + strings.ReplaceAll(s.Origin, "'", "'\"'\"'") + "' --code " + code
	}
	return r
}
func replaceKey(q tx, c Row) (string, Row) {
	a := q.one("SELECT * FROM agents WHERE token_hash=? AND revoked=0", c["token_hash"])
	allowed(a != nil)
	key := "ms_" + token(32)
	hash := digest(key)
	q.exec("UPDATE agents SET revoked=1 WHERE token_hash=?", c["token_hash"])
	q.exec("INSERT INTO agents(token_hash,agent_id,user_id,projects,global_access,sensitive_access,revoked,read_access,write_access) VALUES(?,?,?,?,?,?,0,?,?)", hash, a["agent_id"], a["user_id"], a["projects"], a["global_access"], a["sensitive_access"], a["read_access"], a["write_access"])
	q.exec("UPDATE connections SET token_hash=? WHERE id=?", hash, c["id"])
	q.exec("DELETE FROM setup_codes WHERE connection_id=?", c["id"])
	return key, a
}
func (s *Service) exchange(q tx, code string) Row {
	c := q.one("SELECT c.* FROM setup_codes s JOIN connections c ON c.id=s.connection_id AND c.token_hash=s.token_hash WHERE s.digest=? AND s.expires_at>? AND s.used_at IS NULL AND c.revoked_at IS NULL", digest(code), now())
	allowed(c != nil)
	key, a := replaceKey(q, c)
	r := permissions(a)
	delete(r, "sensitive_access")
	r["connection_id"] = c["id"]
	r["name"] = c["name"]
	r["kind"] = c["kind"]
	r["key"] = key
	r["url"] = s.Origin + "/mcp/"
	q.event(str(c["user_id"]), "agent.setup_completed", str(c["id"]), nil)
	return r
}
func agent(q tx, key string) Row {
	a := q.one("SELECT * FROM agents WHERE token_hash=? AND revoked=0", digest(key))
	if a == nil {
		fail(401, "Agent Key 无效或已撤销")
	}
	return a
}
func scope(a Row, project any) {
	if project == nil {
		allowed(truth(a["global_access"]))
	} else {
		allowed(in(str(project), texts(parseJSON(str(a["projects"])))))
	}
}
func hosted(q tx, uid, secret string) Row {
	expected := os.Getenv("MEMOSPHERE_HOSTED_SECRET")
	allowed(strings.IndexFunc(expected, func(r rune) bool { return r > 127 }) < 0 && len(expected) >= 32 && hmac.Equal([]byte(secret), []byte(expected)))
	q.exec("CREATE TABLE IF NOT EXISTS hosted_grants(user_id TEXT PRIMARY KEY REFERENCES users(id),connection_id TEXT UNIQUE NOT NULL,project_id TEXT NOT NULL,key_nonce TEXT NOT NULL)")
	old := q.one("SELECT * FROM hosted_grants WHERE user_id=?", uid)
	var id, p, nonce string
	if old != nil {
		id, p, nonce = str(old["connection_id"]), str(old["project_id"]), str(old["key_nonce"])
	} else {
		id, p, nonce = uuid.NewString(), uuid.NewString(), hexToken(32)
	}
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte("hosted-key-v1/" + id + "/" + nonce))
	key := "ms_" + hex.EncodeToString(h.Sum(nil))
	hash := digest(key)
	if old != nil {
		allowed(q.one("SELECT 1 FROM connections c JOIN agents a ON a.token_hash=c.token_hash AND a.user_id=c.user_id WHERE c.id=? AND c.user_id=? AND c.token_hash=? AND c.revoked_at IS NULL AND a.revoked=0", id, uid, hash) != nil)
	} else {
		q.exec("INSERT INTO projects VALUES(?,?,?,?)", p, uid, "KoKo", now())
		q.exec("INSERT INTO agents(token_hash,agent_id,user_id,projects,global_access,sensitive_access,revoked,read_access,write_access) VALUES(?,?,?,?,0,0,0,1,1)", hash, id, uid, encoded([]string{p}))
		q.exec("INSERT INTO connections VALUES(?,?,?,?,?,?,NULL,NULL,NULL)", id, uid, "KoKo 云端", "other", hash, now())
		q.exec("INSERT INTO hosted_grants VALUES(?,?,?,?)", uid, id, p, nonce)
		q.event(uid, "project.created", p, nil)
		q.event(uid, "agent.created", id, Row{"name": "KoKo 云端", "kind": "other"})
	}
	return Row{"key": key, "connection_id": id, "project_id": p}
}
