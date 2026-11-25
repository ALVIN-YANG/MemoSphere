"""Agent 身份、独立 Key、授权与一次性接入码。"""

import json
import secrets
import time
import uuid


class Agents:
    def __init__(self, database, spaces):
        self.db, self.spaces = database, spaces

    def list(self, user):
        with self.db.connect() as db:
            rows = db.execute("""SELECT c.*,a.projects,a.global_access,a.sensitive_access,a.read_access,a.write_access
                FROM connections c JOIN agents a ON a.token_hash=c.token_hash WHERE c.user_id=? ORDER BY c.created_at DESC""", (user,))
            return [self.public(row) for row in rows]

    @staticmethod
    def public(row):
        value = dict(row)
        value.pop("token_hash", None)
        value["projects"] = json.loads(value["projects"])
        for key in ["global_access", "sensitive_access", "read_access", "write_access"]:
            value[key] = bool(value[key])
        return value

    def create(self, user, data):
        name = str(data.get("name", "")).strip()
        kind = data.get("kind")
        projects = data.get("projects", [])
        if not 1 <= len(name) <= 80 or kind not in {"codex", "kimi", "other"}:
            raise ValueError("请输入 Agent 名称并选择类型")
        if not projects:raise ValueError("请先选择一个项目")
        self.spaces.check_projects(user, projects)
        connection = str(uuid.uuid4())
        # 初始凭证不交给客户端；一次性接入码兑换时签发实际使用的 Key。
        key = self.db.issue(connection, user, projects, prefix="ms_",
                            global_access=bool(data.get("global_access", True)),
                            sensitive_access=bool(data.get("sensitive_access", False)))
        with self.db.connect() as db:
            db.execute("INSERT INTO connections VALUES (?,?,?,?,?,?,NULL,NULL,NULL)",
                       (connection, user, name, kind, self.db.digest(key), time.time()))
            db.execute("UPDATE agents SET read_access=?,write_access=? WHERE token_hash=?",
                       (bool(data.get("read_access", True)), bool(data.get("write_access", True)), self.db.digest(key)))
            self.db.event(db, user, "agent.created", connection, name=name, kind=kind)
        return next(a for a in self.list(user) if a["id"] == connection)

    def owned(self, db, user, connection):
        row = db.execute("SELECT * FROM connections WHERE id=? AND user_id=?", (connection, user)).fetchone()
        if not row:raise LookupError("Agent 不存在")
        return dict(row)

    def update(self, user, connection, data):
        projects = data.get("projects", [])
        self.spaces.check_projects(user, projects)
        if not projects and not data.get("global_access"):
            raise ValueError("至少授权一个项目或个人全局记忆")
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = self.owned(db, user, connection)
            if row["revoked_at"]:raise ValueError("Agent 已撤销，请创建新的连接")
            db.execute("""UPDATE agents SET projects=?,global_access=?,sensitive_access=?,read_access=?,write_access=?
                WHERE token_hash=?""", (json.dumps(projects), bool(data.get("global_access")),
                bool(data.get("sensitive_access")), bool(data.get("read_access")), bool(data.get("write_access")), row["token_hash"]))
            self.db.event(db, user, "agent.permissions_updated", connection)

    def revoke(self, user, connection):
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = self.owned(db, user, connection)
            db.execute("UPDATE agents SET revoked=1 WHERE token_hash=?", (row["token_hash"],))
            db.execute("UPDATE connections SET revoked_at=? WHERE id=?", (time.time(), connection))
            db.execute("DELETE FROM setup_codes WHERE connection_id=?", (connection,))
            self.db.event(db, user, "agent.revoked", connection)

    def setup_code(self, user, connection):
        code = "pair_" + secrets.token_urlsafe(24)
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = self.owned(db, user, connection)
            if row["revoked_at"]:raise ValueError("Agent 已撤销")
            db.execute("DELETE FROM setup_codes WHERE connection_id=? OR expires_at<?", (connection, time.time()))
            db.execute("INSERT INTO setup_codes VALUES (?,?,?,?,NULL)",
                       (self.db.digest(code), connection, row["token_hash"], time.time() + 600))
            self.db.event(db, user, "agent.setup_created", connection)
        return code

    def replace_key(self, db, row):
        old = db.execute("SELECT * FROM agents WHERE token_hash=? AND revoked=0", (row["token_hash"],)).fetchone()
        if not old:raise PermissionError("Agent 已撤销")
        key = "ms_" + secrets.token_urlsafe(32)
        db.execute("UPDATE agents SET revoked=1 WHERE token_hash=?", (row["token_hash"],))
        db.execute("""INSERT INTO agents
            (token_hash,agent_id,user_id,projects,global_access,sensitive_access,revoked,read_access,write_access)
            VALUES (?,?,?,?,?,?,0,?,?)""", (self.db.digest(key), old["agent_id"], old["user_id"], old["projects"],
            old["global_access"], old["sensitive_access"], old["read_access"], old["write_access"]))
        db.execute("UPDATE connections SET token_hash=? WHERE id=?", (self.db.digest(key), row["id"]))
        db.execute("DELETE FROM setup_codes WHERE connection_id=?", (row["id"],))
        return key, dict(old)

    def rotate(self, user, connection):
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = self.owned(db, user, connection)
            if row["revoked_at"]:raise ValueError("Agent 已撤销")
            key, _ = self.replace_key(db, row)
            self.db.event(db, user, "agent.key_rotated", connection)
        return key

    def exchange(self, code):
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            setup = db.execute("SELECT * FROM setup_codes WHERE digest=? AND expires_at>? AND used_at IS NULL",
                               (self.db.digest(code), time.time())).fetchone()
            if not setup:raise PermissionError("接入码无效、已使用或已过期")
            row = db.execute("SELECT * FROM connections WHERE id=? AND revoked_at IS NULL", (setup["connection_id"],)).fetchone()
            if not row or row["token_hash"] != setup["token_hash"]:
                raise PermissionError("接入授权已变更，请重新生成接入码")
            row = dict(row)
            key, grant = self.replace_key(db, row)
            self.db.event(db, row["user_id"], "agent.setup_completed", row["id"])
        return {"connection_id": row["id"], "name": row["name"], "kind": row["kind"],
                "key": key, "projects": json.loads(grant["projects"]),
                "global_access": bool(grant["global_access"]),
                "read_access": bool(grant["read_access"]), "write_access": bool(grant["write_access"])}
