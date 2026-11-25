package memory

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// PrepareApplicationDB separates application subjects from standalone console owners.
func (db DB) PrepareApplicationDB(ctx context.Context) (err error) {
	defer catch(&err)
	db.transaction(ctx, true, func(q tx) any {
		if q.count("SELECT count(*) FROM users") != 0 {
			fail(409, "应用模式需要独立数据库，请从副本导入专属项目")
		}
		if q.count("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='application_spaces'") == 0 {
			if q.count("SELECT (SELECT count(*) FROM projects)+(SELECT count(*) FROM memories)+(SELECT count(*) FROM messages)+(SELECT count(*) FROM agents)") != 0 {
				fail(409, "目标库不是空的应用数据库")
			}
			q.exec(appSpacesDDL)
		}
		return nil
	})
	return nil
}
func (db DB) IsApplicationDB() (bool, error) {
	var n int
	err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='application_spaces'").Scan(&n)
	return n != 0, err
}

// CheckDatabaseMode runs before schema upgrades so pointing a service at the wrong
// database does not first migrate the other service's store.
func CheckDatabaseMode(path string, application bool) error {
	info, e := os.Stat(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if info.Size() == 0 {
		return nil
	}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	u := url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro"}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return e
	}
	defer db.Close()
	exists := func(table string) (bool, error) {
		var n int
		e := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n)
		return n > 0, e
	}
	marked, e := exists("application_spaces")
	if e != nil {
		return e
	}
	if marked && !application {
		return errors.New("此库属于应用模式，请使用 --application")
	}
	if !marked && application {
		for _, name := range []string{"users", "projects", "memories", "agents", "messages"} {
			ok, e := exists(name)
			if e != nil {
				return e
			}
			if !ok {
				continue
			}
			var n int
			if e = db.QueryRow("SELECT EXISTS(SELECT 1 FROM " + name + ")").Scan(&n); e != nil {
				return e
			}
			if n != 0 {
				return errors.New("应用模式需要独立数据库，请从旧库副本导入")
			}
		}
	}
	return nil
}
