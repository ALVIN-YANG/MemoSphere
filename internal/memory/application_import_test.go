package memory

import (
	"path/filepath"
	"testing"
)

func TestImportOnlyBoundProjectAndTombstones(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.sqlite")
	source, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer source.Close()
	service, e := New(source, "http://localhost", nil)
	if e != nil {
		t.Fatal(e)
	}
	const user = "original"
	var projectID, connection, hotel, private string
	source.transaction(ctx, true, func(q tx) any {
		q.exec("INSERT INTO users VALUES(?,?,?,?)", user, "owner@example.test", "unused", now())
		projectID = str(project(q, user, "旧应用项目")["id"])
		a := createAgent(q, user, Row{"name": "旧客户端", "kind": "other", "projects": []string{projectID}, "global_access": false})
		connection = str(a["id"])
		key := str(service.exchange(q, str(service.setup(q, user, connection)["code"]))["key"])
		hotel = str(save(q, user, "", Row{"project_id": projectID, "text": "酒店在湖边"})["id"])
		private = str(save(q, user, "", Row{"text": "独立控制台的私人记忆"})["id"])
		m := service.record(q, key, Row{"project_id": projectID, "session_id": "s", "source_id": "original", "role": "user", "text": "酒店在湖边"})
		q.exec("INSERT INTO memory_sources(memory_id,revision,message_id,quote) VALUES(?,1,?,'酒店在湖边')", hotel, m["id"])
		q.exec("INSERT INTO deleted_sources VALUES(?,?,?,?)", user, connection, "s", "forgotten")
		return nil
	})
	if err := CheckDatabaseMode(path, true); err == nil {
		t.Fatal("would migrate existing console DB in application mode")
	}
	dest, e := Open(filepath.Join(dir, "app.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer dest.Close()
	if err := dest.ImportApplicationProject(ctx, path, "account-A", projectID, connection); err != nil {
		t.Fatal(err)
	}
	if err := dest.ImportApplicationProject(ctx, path, "account-A", projectID, connection); err != nil {
		t.Fatal("retry", err)
	}
	if err := dest.ImportApplicationProject(ctx, path, "account-B", projectID, connection); err == nil {
		t.Fatal("duplicate mapping")
	}
	core, e := New(dest, "http://localhost", nil)
	if e != nil {
		t.Fatal(e)
	}
	dest.transaction(ctx, true, func(q tx) any {
		space := appSpace(q, "account-A", projectID)
		if q.count("SELECT count(*) FROM users") != 0 || q.one("SELECT 1 FROM memories WHERE id=?", private) != nil {
			t.Fatal("standalone identity or memory imported")
		}
		r := detail(q, str(space["owner"]), hotel)
		if len(r["sources"].([]Row)) != 1 || r["project_id"] != projectID {
			t.Fatal("wrong project evidence")
		}
		for _, id := range []string{"original", "forgotten"} {
			m := core.record(q, str(space["agent_key"]), Row{"project_id": projectID, "session_id": "s", "source_id": id, "role": "user", "text": "酒店在湖边"})
			if !truth(m["duplicate"]) || (id == "forgotten") != truth(m["deleted"]) {
				t.Fatal("lost replay state", id)
			}
		}
		return nil
	})
	// A fact with a dependency on an independent private memory cannot be imported piecemeal.
	source.transaction(ctx, true, func(q tx) any {
		q.exec("INSERT INTO memory_dependencies VALUES(?,?,?,?)", hotel, 1, private, 1)
		return nil
	})
	rejected, e := Open(filepath.Join(dir, "rejected.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer rejected.Close()
	if err := rejected.ImportApplicationProject(ctx, path, "other-install", projectID, connection); err == nil {
		t.Fatal("imported cross-project dependency")
	}
	rejected.transaction(ctx, false, func(q tx) any {
		if q.count("SELECT count(*) FROM application_spaces") != 0 || q.count("SELECT count(*) FROM memories") != 0 {
			t.Fatal("failed import left partial data")
		}
		return nil
	})
	if app, err := source.IsApplicationDB(); err != nil || app {
		t.Fatal("source mode changed", err)
	}
	if err := source.PrepareApplicationDB(ctx); err == nil {
		t.Fatal("accepted standalone database")
	}
}

func TestImportRevokedHostedGrantRetainsOwnerDataOnly(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	src := filepath.Join(dir, "old.sqlite")
	old, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	dst, err := Open(filepath.Join(dir, "dest.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var connection, projectID, item string
	old.transaction(ctx, true, func(q tx) any {
		q.exec("INSERT INTO users VALUES('owner','owner@example.test','unused',?)", now())
		projectID = str(project(q, "owner", "旧项目")["id"])
		a := createAgent(q, "owner", Row{"name": "旧助理", "kind": "other", "projects": []string{projectID}, "global_access": false})
		connection = str(a["id"])
		item = str(save(q, "owner", "", Row{"project_id": projectID, "text": "仅所有者可见"})["id"])
		revoke(q, "owner", connection)
		return nil
	})
	if err = dst.ImportApplicationProject(ctx, src, "account", projectID, connection); err == nil {
		t.Fatal("missing historical binding accepted")
	}
	old.transaction(ctx, true, func(q tx) any {
		q.exec("CREATE TABLE hosted_grants(user_id TEXT PRIMARY KEY,connection_id TEXT UNIQUE,project_id TEXT,key_nonce TEXT)")
		q.exec("INSERT INTO hosted_grants VALUES(?,?,?,'unused')", "owner", connection, projectID)
		return nil
	})
	for i := 0; i < 2; i++ {
		if err = dst.ImportApplicationProject(ctx, src, "account", projectID, connection); err != nil {
			t.Fatal(err)
		}
	}
	dst.transaction(ctx, false, func(q tx) any {
		space := appSpace(q, "account", projectID)
		if detail(q, str(space["owner"]), item)["id"] != item || q.count("SELECT count(*) FROM agents WHERE revoked=0") != 0 || q.count("SELECT count(*) FROM connections WHERE revoked_at IS NULL") != 0 {
			t.Fatal("revoked grant revived or owner data lost")
		}
		return nil
	})
}
