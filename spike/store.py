"""最小持久化与授权边界，用真实 HTTP 测试证明隔离和撤销。"""

import hashlib
import json
import secrets
import sqlite3
import time
from contextlib import contextmanager
from pathlib import Path


class Store:
    def __init__(self, path: Path):
        self.path = path
        path.parent.mkdir(parents=True, exist_ok=True)
        with self.connect() as db:
            db.executescript("""
                CREATE TABLE IF NOT EXISTS agents (
                    token_hash TEXT PRIMARY KEY, agent_id TEXT NOT NULL,
                    user_id TEXT NOT NULL, projects TEXT NOT NULL,
                    global_access INTEGER NOT NULL, sensitive_access INTEGER NOT NULL,
                    revoked INTEGER NOT NULL DEFAULT 0
                );
                CREATE TABLE IF NOT EXISTS messages (
                    id INTEGER PRIMARY KEY, user_id TEXT NOT NULL,
                    agent_id TEXT NOT NULL, session_id TEXT NOT NULL,
                    source_id TEXT NOT NULL, project_id TEXT,
                    role TEXT NOT NULL, text TEXT NOT NULL,
                    sensitive INTEGER NOT NULL, recorded_at REAL NOT NULL,
                    UNIQUE(user_id, agent_id, session_id, source_id)
                );
                CREATE INDEX IF NOT EXISTS messages_scope ON messages(user_id, project_id, id);
            """)

            if "spoken_at" not in {r[1] for r in db.execute("PRAGMA table_info(messages)")}:
                db.execute("ALTER TABLE messages ADD COLUMN spoken_at TEXT")

    @contextmanager
    def connect(self):
        db = sqlite3.connect(self.path, timeout=5)
        db.row_factory = sqlite3.Row
        try:
            with db:
                yield db
        finally:
            db.close()

    def issue(self, agent_id, user_id, projects, *, global_access=True, sensitive_access=False, prefix="ms_test_"):
        token = prefix + secrets.token_urlsafe(32)
        with self.connect() as db:
            db.execute("INSERT INTO agents (token_hash,agent_id,user_id,projects,global_access,sensitive_access,revoked) VALUES (?, ?, ?, ?, ?, ?, 0)", (
                self.digest(token), agent_id, user_id, json.dumps(projects),
                global_access, sensitive_access,
            ))
        return token

    @staticmethod
    def digest(token):
        return hashlib.sha256(token.encode()).hexdigest()

    def authenticate(self, token):
        with self.connect() as db:
            row = db.execute("SELECT * FROM agents WHERE token_hash=? AND revoked=0",
                             (self.digest(token),)).fetchone()
        if row is None:
            raise PermissionError("Agent Key 无效或已撤销")
        return dict(row)

    def revoke(self, token):
        with self.connect() as db:
            db.execute("UPDATE agents SET revoked=1 WHERE token_hash=?", (self.digest(token),))

    def after_record(self, db, message_id):
        """正式服务可在消息事务内持久化后续处理任务。"""

    def suppressed_source(self, db, agent, session_id, source_id):
        return False

    @staticmethod
    def check_project(agent, project_id):
        if project_id is None:
            if not agent["global_access"]:
                raise PermissionError("无全局记忆权限")
        elif project_id not in json.loads(agent["projects"]):
            raise PermissionError("无此项目权限")

    def record(self, token, *, project_id, session_id, source_id, role, text, sensitive=False, spoken_at=None, connection_id=None):
        if spoken_at is not None:
            from datetime import datetime
            if not isinstance(spoken_at,str) or len(spoken_at)>40 or datetime.fromisoformat(spoken_at).utcoffset() is None:
                raise ValueError("原话时间需要带时区的 ISO 8601 时间")
        if role not in {"user", "assistant", "tool"}:
            raise ValueError("只接收用户、助手和工具消息")
        if not text.strip() or len(text.encode()) > 262144:
            raise ValueError("消息为空或超过验证服务单条容量")
        if not session_id or not source_id or max(len(session_id), len(source_id)) > 256:
            raise ValueError("缺少有效来源标识")
        # 鉴权与写入在同一事务内，避免撤销与写入交错。
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT * FROM agents WHERE token_hash=? AND revoked=0",
                             (self.digest(token),)).fetchone()
            if row is None:
                raise PermissionError("Agent Key 无效或已撤销")
            agent = dict(row)
            if connection_id is not None and agent["agent_id"] != connection_id:
                raise PermissionError("连接与密钥不匹配")
            if not agent.get("write_access", 1):
                raise PermissionError("此 Agent 没有写入权限")
            self.check_project(agent, project_id)
            if sensitive and not agent["sensitive_access"]:
                raise PermissionError("无敏感记忆权限")
            if self.suppressed_source(db, agent, session_id, source_id):
                return {"id": None, "duplicate": True, "deleted": True}
            existing = db.execute("""SELECT * FROM messages WHERE
                user_id=? AND agent_id=? AND session_id=? AND source_id=?""",
                (agent["user_id"], agent["agent_id"], session_id, source_id)).fetchone()
            if existing:
                if (existing["text"], existing["project_id"], existing["role"], existing["sensitive"], existing["spoken_at"]) != (
                        text, project_id, role, sensitive, spoken_at):
                    raise ValueError("同一来源标识不能对应不同内容")
                return {"id": existing["id"], "duplicate": True}
            result = db.execute("""INSERT INTO messages
                (user_id, agent_id, session_id, source_id, project_id, role, text, sensitive, recorded_at, spoken_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (agent["user_id"], agent["agent_id"], session_id, source_id, project_id,
                 role, text, sensitive, time.time(), spoken_at))
            self.after_record(db, result.lastrowid)
            return {"id": result.lastrowid, "duplicate": False}

    def read(self, token, project_id, *, query="", limit=10, exclude_session=None):
        agent = self.authenticate(token)
        if not agent.get("read_access", 1):
            raise PermissionError("此 Agent 没有读取权限")
        self.check_project(agent, project_id)
        conditions = ["user_id=?"]
        args = [agent["user_id"]]
        if project_id is not None and agent["global_access"]:
            conditions.append("(project_id=? OR project_id IS NULL)")
            args.append(project_id)
        elif project_id is None:
            conditions.append("project_id IS NULL")
        else:
            conditions.append("project_id=?")
            args.append(project_id)
        if not agent["sensitive_access"]:
            conditions.append("sensitive=0")
        if exclude_session:
            conditions.append("NOT (agent_id=? AND session_id=?)")
            args.extend([agent["agent_id"], exclude_session])
        if query:
            conditions.append("instr(lower(text), lower(?)) > 0")
            args.append(query)
        with self.connect() as db:
            rows = db.execute("SELECT id,agent_id,session_id,source_id,project_id,role,text,recorded_at "
                              "FROM messages WHERE " + " AND ".join(conditions) +
                              " ORDER BY id DESC LIMIT ?", (*args, max(1, min(limit, 20)))).fetchall()
        return [dict(row) for row in reversed(rows)]
