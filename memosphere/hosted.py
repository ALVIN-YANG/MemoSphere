"""A verified owner grants the configured hosted assistant its own project."""

import hashlib
import hmac
import json
import os
import secrets
import time
import uuid


def grant(database, user_id, client_secret):
    secret = os.environ.get("MEMOSPHERE_HOSTED_SECRET", "")
    if (
        not secret.isascii()
        or len(secret) < 32
        or not hmac.compare_digest(secret.encode(), client_secret.encode())
    ):
        raise PermissionError("托管客户端未获授权")
    with database.connect() as db:
        db.execute("BEGIN IMMEDIATE")
        db.execute("""CREATE TABLE IF NOT EXISTS hosted_grants(
            user_id TEXT PRIMARY KEY REFERENCES users(id), connection_id TEXT UNIQUE NOT NULL,
            project_id TEXT NOT NULL, key_nonce TEXT NOT NULL)""")
        old = db.execute(
            "SELECT connection_id,project_id,key_nonce FROM hosted_grants WHERE user_id=?",
            (user_id,),
        ).fetchone()
        if old:
            connection, project, nonce = old
        else:
            connection, project, nonce = (
                str(uuid.uuid4()),
                str(uuid.uuid4()),
                secrets.token_hex(32),
            )
        # A retry can reproduce only this connection's key. Revocation and rotation are authoritative.
        key = (
            "ms_"
            + hmac.new(
                secret.encode(),
                ("hosted-key-v1/" + connection + "/" + nonce).encode(),
                hashlib.sha256,
            ).hexdigest()
        )
        digest = database.digest(key)
        if old:
            valid = db.execute(
                """SELECT 1 FROM connections c JOIN agents a ON a.token_hash=c.token_hash
                WHERE c.id=? AND c.user_id=? AND a.user_id=c.user_id AND c.token_hash=?
                AND c.revoked_at IS NULL AND a.revoked=0""",
                (connection, user_id, digest),
            ).fetchone()
            if not valid:
                raise PermissionError("托管连接已撤销或更换密钥")
        else:
            now = time.time()
            db.execute(
                "INSERT INTO projects(id,user_id,name,created_at) VALUES(?,?,?,?)",
                (project, user_id, "KoKo", now),
            )
            db.execute(
                """INSERT INTO agents(token_hash,agent_id,user_id,projects,global_access,sensitive_access,revoked,read_access,write_access)
                VALUES(?,?,?,?,0,0,0,1,1)""",
                (digest, connection, user_id, json.dumps([project])),
            )
            db.execute(
                "INSERT INTO connections(id,user_id,name,kind,token_hash,created_at) VALUES(?,?,?,?,?,?)",
                (connection, user_id, "KoKo 云端", "other", digest, now),
            )
            db.execute(
                "INSERT INTO hosted_grants VALUES(?,?,?,?)",
                (user_id, connection, project, nonce),
            )
            database.event(db, user_id, "project.created", project)
            database.event(
                db, user_id, "agent.created", connection, name="KoKo 云端", kind="other"
            )
        return {"key": key, "connection_id": connection, "project_id": project}
