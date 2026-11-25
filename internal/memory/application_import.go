package memory

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ImportApplicationProject works on a consistent private copy; source schemas may
// predate evidence or compaction. All destination writes commit together.
func (db DB) ImportApplicationProject(ctx context.Context, source, subject, projectID, connection string) (err error) {
	defer catch(&err)
	if strings.TrimSpace(subject) == "" || len(subject) > 200 || projectID == "" || connection == "" {
		return errors.New("缺少用户、项目或连接标识")
	}
	if err = db.PrepareApplicationDB(ctx); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "memosphere-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	path, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	src, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return err
	}
	defer src.Close()
	copyPath := filepath.Join(temp, "source.sqlite")
	if _, err = src.ExecContext(ctx, "VACUUM INTO ?", copyPath); err != nil {
		return err
	}
	upgraded, err := Open(copyPath)
	if err != nil {
		return err
	}
	if err = upgraded.Close(); err != nil {
		return err
	}
	// ATTACH is connection-local. Reserve one connection and always detach after rollback/commit.
	con, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer con.Close()
	if _, err = con.ExecContext(ctx, "ATTACH DATABASE ? AS prior", copyPath); err != nil {
		return err
	}
	defer con.ExecContext(context.Background(), "DETACH DATABASE prior")
	q := tx{ctx, con}
	q.exec("BEGIN IMMEDIATE")
	defer con.ExecContext(context.Background(), "ROLLBACK")
	old := q.one(`SELECT p.user_id,p.name,p.created_at,c.revoked_at,a.revoked,a.global_access,a.sensitive_access,a.read_access,a.write_access FROM prior.projects p JOIN prior.connections c ON c.user_id=p.user_id
 JOIN prior.agents a ON a.token_hash=c.token_hash AND a.user_id=p.user_id AND a.agent_id=c.id
 WHERE p.id=? AND c.id=? AND ((c.revoked_at IS NULL AND a.revoked=0) OR (c.revoked_at IS NOT NULL AND a.revoked=1)) AND EXISTS(SELECT 1 FROM json_each(a.projects) WHERE value=p.id)`, projectID, connection)
	if old == nil {
		fail(409, "源连接没有该项目的有效授权")
	}
	owner := str(old["user_id"])
	if truth(old["revoked"]) {
		// A revoked token cannot prove ownership. A persisted historical hosted binding can.
		if q.count("SELECT count(*) FROM prior.sqlite_master WHERE type='table' AND name='hosted_grants'") == 0 ||
			q.one("SELECT 1 FROM prior.hosted_grants WHERE user_id=? AND connection_id=? AND project_id=?", owner, connection, projectID) == nil {
			fail(409, "撤权连接缺少既有托管归属证明")
		}
	}
	preserveGrant := func(first bool) {
		// Never resurrect a revoked connection on a retry. The owner can still manage their facts.
		q.exec("UPDATE connections SET revoked_at=coalesce(revoked_at,?) WHERE id=? AND user_id=?", old["revoked_at"], connection, owner)
		hash := q.one("SELECT token_hash FROM connections WHERE id=? AND user_id=?", connection, owner)["token_hash"]
		if first {
			q.exec("UPDATE agents SET global_access=?,sensitive_access=?,read_access=?,write_access=?,revoked=? WHERE token_hash=?", old["global_access"], old["sensitive_access"], old["read_access"], old["write_access"], old["revoked"], hash)
		} else {
			q.exec("UPDATE agents SET global_access=min(global_access,?),sensitive_access=min(sensitive_access,?),read_access=min(read_access,?),write_access=min(write_access,?),revoked=max(revoked,?) WHERE token_hash=?", old["global_access"], old["sensitive_access"], old["read_access"], old["write_access"], old["revoked"], hash)
		}
	}
	existing := q.one("SELECT * FROM application_spaces WHERE subject=?", subject)
	if existing != nil {
		if existing["owner"] != owner || existing["project"] != projectID || existing["connection"] != connection {
			fail(409, "目标用户已有不同空间")
		}
		preserveGrant(false)
		q.exec("COMMIT")
		return nil
	}
	// Never pull another project's source text, historical version or inference across the boundary.
	facts := "SELECT id FROM prior.memories WHERE user_id=? AND project_id=?"
	msgs := "SELECT id FROM prior.messages WHERE user_id=? AND project_id=? AND agent_id=?"
	if q.count("SELECT count(*) FROM prior.memory_sources WHERE memory_id IN ("+facts+") AND message_id NOT IN ("+msgs+")", owner, projectID, owner, projectID, connection) > 0 ||
		q.count("SELECT count(*) FROM prior.memory_dependencies WHERE child IN ("+facts+") AND parent NOT IN ("+facts+")", owner, projectID, owner, projectID) > 0 ||
		q.count("SELECT count(*) FROM prior.memory_versions WHERE memory_id IN ("+facts+") AND (json_extract(content,'$.project_id') IS NOT ? OR json_extract(content,'$.user_id') IS NOT ?)", owner, projectID, projectID, owner) > 0 ||
		q.count("SELECT count(*) FROM prior.memory_sources WHERE message_id IN ("+msgs+") AND memory_id NOT IN ("+facts+")", owner, projectID, connection, owner, projectID) > 0 ||
		q.count("SELECT count(*) FROM prior.messages WHERE user_id=? AND agent_id=? AND project_id IS NOT ?", owner, connection, projectID) > 0 {
		fail(409, "来源或版本跨项目，需要先核对数据归属")
	}
	q.exec("INSERT INTO projects(id,user_id,name,created_at) VALUES(?,?,?,?)", projectID, owner, old["name"], old["created_at"])
	insertAppSpace(q, subject, owner, projectID, connection, "internal_"+token(32))
	preserveGrant(true)
	copyRows := func(table, filter string, args ...any) {
		cols := []string{}
		for _, r := range q.rows("PRAGMA main.table_info(" + table + ")") {
			cols = append(cols, str(r["name"]))
		}
		names := strings.Join(cols, ",")
		q.exec("INSERT INTO main."+table+"("+names+") SELECT "+names+" FROM prior."+table+" WHERE "+filter, args...)
	}
	copyRows("messages", "id IN ("+msgs+")", owner, projectID, connection)
	copyRows("memories", "id IN ("+facts+")", owner, projectID)
	for _, table := range []string{"memory_versions", "memory_sources", "memory_entities", "entity_bindings", "memory_vectors"} {
		copyRows(table, "memory_id IN ("+facts+")", owner, projectID)
	}
	for _, table := range []string{"memory_dependencies", "memory_reviews"} {
		copyRows(table, "child IN ("+facts+")", owner, projectID)
	}
	copyRows("retain_operations", "user_id=? AND agent_id=? AND project_id=?", owner, connection, projectID)
	copyRows("retain_items", "operation_id IN (SELECT id FROM prior.retain_operations WHERE user_id=? AND agent_id=? AND project_id=?) AND message_id IN ("+msgs+")", owner, connection, projectID, owner, projectID, connection)
	copyRows("retain_admissions", "message_id IN ("+msgs+")", owner, projectID, connection)
	copyRows("extraction_jobs", "message_id IN ("+msgs+")", owner, projectID, connection)
	// Usage IDs are local counters; preserve usage values, not their row numbers.
	q.exec("INSERT INTO model_usage(message_id,model,input_tokens,output_tokens,reasoning_tokens,latency,created_at,outcome,purpose) SELECT message_id,model,input_tokens,output_tokens,reasoning_tokens,latency,created_at,outcome,purpose FROM prior.model_usage WHERE message_id IN ("+msgs+")", owner, projectID, connection)
	copyRows("deleted_sources", "user_id=? AND agent_id=?", owner, connection)
	stream := "SELECT id FROM prior.conversation_streams WHERE user_id=? AND project_id=? AND agent_id=?"
	copyRows("conversation_streams", "id IN ("+stream+")", owner, projectID, connection)
	copyRows("compaction_jobs", "stream_id IN ("+stream+")", owner, projectID, connection)
	q.bump(owner)
	q.exec("COMMIT")
	return nil
}
