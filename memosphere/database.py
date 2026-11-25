"""单进程服务的数据边界；开发数据库与接入验证数据库独立。"""

import json
import time
from pathlib import Path

from spike.store import Store


class Database(Store):
    def __init__(self, path):
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        # 在 SQLite 建 WAL 文件前设置数据库权限，避免跟随宽松的默认 umask。
        path.touch(mode=0o600, exist_ok=True)
        path.chmod(0o600)
        super().__init__(Path(path))
        with self.connect() as db:
            db.execute("PRAGMA journal_mode=WAL")
            columns = {r[1] for r in db.execute("PRAGMA table_info(agents)")}
            for column in ["read_access", "write_access"]:
                if column not in columns:
                    db.execute(f"ALTER TABLE agents ADD COLUMN {column} INTEGER NOT NULL DEFAULT 1")
            db.executescript("""
                CREATE TABLE IF NOT EXISTS users (
                    id TEXT PRIMARY KEY, email TEXT UNIQUE NOT NULL,
                    password_hash TEXT NOT NULL, created_at REAL NOT NULL
                );
                CREATE TABLE IF NOT EXISTS email_challenges (
                    email TEXT NOT NULL, purpose TEXT NOT NULL, requested_at REAL NOT NULL,
                    delivery_id TEXT, code_hash TEXT, expires_at REAL, attempts INTEGER NOT NULL DEFAULT 0,
                    PRIMARY KEY(email, purpose)
                );
                CREATE TABLE IF NOT EXISTS sessions (
                    token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL,
                    csrf TEXT NOT NULL, expires_at REAL NOT NULL
                );
                CREATE TABLE IF NOT EXISTS projects (
                    id TEXT PRIMARY KEY, user_id TEXT NOT NULL,
                    name TEXT NOT NULL, created_at REAL NOT NULL
                );
                CREATE TABLE IF NOT EXISTS connections (
                    id TEXT PRIMARY KEY, user_id TEXT NOT NULL,
                    name TEXT NOT NULL, kind TEXT NOT NULL, token_hash TEXT NOT NULL,
                    created_at REAL NOT NULL, last_seen REAL,
                    last_ingested REAL, revoked_at REAL
                );
                CREATE TABLE IF NOT EXISTS setup_codes (
                    digest TEXT PRIMARY KEY, connection_id TEXT NOT NULL,
                    token_hash TEXT NOT NULL, expires_at REAL NOT NULL,
                    used_at REAL
                );
                CREATE TABLE IF NOT EXISTS memories (
                    id TEXT PRIMARY KEY, user_id TEXT NOT NULL, project_id TEXT,
                    text TEXT NOT NULL, category TEXT NOT NULL, tags TEXT NOT NULL,
                    sensitive INTEGER NOT NULL, fact_time TEXT NOT NULL,
                    revision INTEGER NOT NULL, created_at REAL NOT NULL,
                    updated_at REAL NOT NULL
                );
                CREATE TABLE IF NOT EXISTS memory_versions (
                    memory_id TEXT NOT NULL, revision INTEGER NOT NULL,
                    content TEXT NOT NULL, reason TEXT NOT NULL,
                    recorded_at REAL NOT NULL, PRIMARY KEY(memory_id, revision)
                );
                CREATE TABLE IF NOT EXISTS events (
                    id INTEGER PRIMARY KEY, user_id TEXT NOT NULL,
                    action TEXT NOT NULL, subject TEXT NOT NULL,
                    details TEXT NOT NULL, created_at REAL NOT NULL
                );
                CREATE INDEX IF NOT EXISTS memories_owner ON memories(user_id,updated_at);
                CREATE INDEX IF NOT EXISTS events_owner ON events(user_id,id);
            """)
            columns = {r[1] for r in db.execute("PRAGMA table_info(memories)")}
            for column, definition in {
                "state": "TEXT NOT NULL DEFAULT 'active'", "origin": "TEXT NOT NULL DEFAULT 'manual'",
                "kind": "TEXT NOT NULL DEFAULT 'profile'", "last_operation": "TEXT NOT NULL DEFAULT 'ADD'",
            }.items():
                if column not in columns:db.execute(f"ALTER TABLE memories ADD COLUMN {column} {definition}")
            from memosphere import evidence
            evidence.migrate(db)
            from memosphere.search_entities import migrate as migrate_search
            migrate_search(db)
            versions = {r[1] for r in db.execute("PRAGMA table_info(memory_versions)")}
            if "disposition" not in versions:
                db.execute("ALTER TABLE memory_versions ADD COLUMN disposition TEXT NOT NULL DEFAULT 'current'")
            if "operation" not in versions:
                db.execute("ALTER TABLE memory_versions ADD COLUMN operation TEXT NOT NULL DEFAULT 'USER_EDIT'")
            db.execute("""UPDATE memory_versions SET disposition='superseded'
                WHERE disposition='current' AND revision < (SELECT revision FROM memories WHERE id=memory_id)""")
            db.executescript("""
                CREATE TABLE IF NOT EXISTS memory_epochs (user_id TEXT PRIMARY KEY, epoch INTEGER NOT NULL);
                CREATE TABLE IF NOT EXISTS memory_sources (
                    memory_id TEXT NOT NULL, revision INTEGER NOT NULL, message_id INTEGER NOT NULL,
                    quote TEXT NOT NULL, PRIMARY KEY(memory_id,revision,message_id,quote)
                );
                CREATE TABLE IF NOT EXISTS extraction_jobs (
                    message_id INTEGER PRIMARY KEY, state TEXT NOT NULL DEFAULT 'pending',
                    attempts INTEGER NOT NULL DEFAULT 0, next_attempt REAL NOT NULL DEFAULT 0,
                    lease_until REAL NOT NULL DEFAULT 0, lease_token TEXT, error TEXT,
                    result TEXT, updated_at REAL NOT NULL
                );
                CREATE TABLE IF NOT EXISTS model_usage (
                    id INTEGER PRIMARY KEY, message_id INTEGER NOT NULL, model TEXT NOT NULL,
                    input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
                    reasoning_tokens INTEGER NOT NULL, latency REAL NOT NULL, created_at REAL NOT NULL
                );
                CREATE TABLE IF NOT EXISTS deleted_sources (
                    user_id TEXT NOT NULL, agent_id TEXT NOT NULL, session_id TEXT NOT NULL,
                    source_id TEXT NOT NULL, PRIMARY KEY(user_id,agent_id,session_id,source_id)
                );
                CREATE INDEX IF NOT EXISTS memory_sources_message ON memory_sources(message_id);
                CREATE INDEX IF NOT EXISTS extraction_pending ON extraction_jobs(state,next_attempt,message_id);
            """)
            if "outcome" not in {r[1] for r in db.execute("PRAGMA table_info(model_usage)")}:
                db.execute("ALTER TABLE model_usage ADD COLUMN outcome TEXT NOT NULL DEFAULT 'unknown'")
            # 已有归档只做迁移标记，不因启用新引擎自动上传旧会话。
            db.execute("""INSERT OR IGNORE INTO extraction_jobs(message_id,state,error,updated_at)
                SELECT id,'skipped','legacy_archive',? FROM messages""", (time.time(),))

    def after_record(self, db, message_id):
        db.execute("INSERT INTO extraction_jobs(message_id,updated_at) VALUES (?,?)", (message_id, time.time()))

    def suppressed_source(self, db, agent, session_id, source_id):
        return bool(db.execute("""SELECT 1 FROM deleted_sources WHERE
            user_id=? AND agent_id=? AND session_id=? AND source_id=?""",
            (agent["user_id"], agent["agent_id"], session_id, source_id)).fetchone())

    @staticmethod
    def event(db, user_id, action, subject="", **details):
        db.execute("INSERT INTO events (user_id,action,subject,details,created_at) VALUES(?,?,?,?,?)",
                   (user_id, action, subject, json.dumps(details, ensure_ascii=False), time.time()))

    def authenticate(self, token):
        agent = super().authenticate(token)
        now = time.time()
        with self.connect() as db:
            db.execute("UPDATE connections SET last_seen=? WHERE token_hash=? AND (last_seen IS NULL OR last_seen<?)",
                       (now, self.digest(token), now - 30))
        return agent

    def record(self, token, **message):
        result = super().record(token, **message)
        with self.connect() as db:
            db.execute("UPDATE connections SET last_ingested=? WHERE token_hash=?", (time.time(), self.digest(token)))
        return result
