"""口令哈希与可撤销的服务端会话，不使用浏览器持久化 bearer token。"""

import hashlib
import hmac
import re
import secrets
import sqlite3
import time
import uuid
from email.headerregistry import Address
from email.errors import HeaderParseError

from memosphere import mail

CODE_TTL = 600
RESEND_AFTER = 60
MAX_ATTEMPTS = 5


class ResendTooSoon(ValueError):
    def __init__(self, seconds):
        self.retry_after = seconds
        super().__init__(f"请在 {seconds} 秒后重新发送")


def password_hash(password, salt=None):
    salt = salt or secrets.token_hex(16)
    digest = hashlib.scrypt(password.encode(), salt=bytes.fromhex(salt), n=32768, r=8, p=1,
                            maxmem=64 * 1024 * 1024, dklen=32)
    return "scrypt$" + salt + "$" + digest.hex()


def password_matches(password, stored):
    try:
        _, salt, _ = stored.split("$")
        return hmac.compare_digest(password_hash(password, salt), stored)
    except (ValueError, TypeError):
        return False


class Accounts:
    def __init__(self, database):
        self.db = database
        self.dummy = password_hash(secrets.token_urlsafe(24))

    @staticmethod
    def email_address(email):
        email = str(email).strip().lower()
        if len(email) > 254 or not re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", email):
            raise ValueError("请输入有效的邮箱地址")
        try:
            Address(addr_spec=email)
        except (HeaderParseError, ValueError):
            raise ValueError("请输入有效的邮箱地址") from None
        return email

    @staticmethod
    def registration_password(password):
        if not isinstance(password, str) or not 10 <= len(password) <= 256:
            raise ValueError("密码需为 10 至 256 个字符")

    def insert_user(self, db, email, hashed):
        user = {"id": str(uuid.uuid4()), "email": email}
        try:
            db.execute(
                "INSERT INTO users VALUES (?,?,?,?)",
                (user["id"], email, hashed, time.time()),
            )
        except sqlite3.IntegrityError:
            raise ValueError("该邮箱已注册，请登录") from None
        self.db.event(db, user["id"], "account.created")
        return user

    def register(self, email, password):
        # Internal creation used by storage fixtures. Public registration uses verified_session.
        email = self.email_address(email)
        self.registration_password(password)
        hashed = password_hash(password)
        with self.db.connect() as db:
            return self.insert_user(db, email, hashed)

    def login(self, email, password):
        if not isinstance(password, str) or len(password) > 256:
            raise ValueError("邮箱或密码不正确")
        with self.db.connect() as db:
            row = db.execute(
                "SELECT * FROM users WHERE email=?", (str(email).strip().lower(),)
            ).fetchone()
        valid = password_matches(password, row["password_hash"] if row else self.dummy)
        if not row or not valid:
            raise ValueError("邮箱或密码不正确")
        return {"id": row["id"], "email": row["email"]}

    def request_code(self, purpose, email, password):
        email = self.email_address(email)
        if purpose == "login":
            self.login(email, password)
        elif purpose == "register":
            self.registration_password(password)
            with self.db.connect() as db:
                if db.execute("SELECT 1 FROM users WHERE email=?", (email,)).fetchone():
                    raise ValueError("该邮箱已注册，请登录")
        else:
            raise ValueError("验证码用途无效")
        now = time.time()
        delivery = secrets.token_hex(16)
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            db.execute(
                "DELETE FROM email_challenges WHERE requested_at<? AND COALESCE(expires_at,0)<=?",
                (now - CODE_TTL, now),
            )
            row = db.execute(
                "SELECT requested_at FROM email_challenges WHERE email=? AND purpose=?",
                (email, purpose),
            ).fetchone()
            previous = row[0] if row else 0
            remaining = previous + RESEND_AFTER - now
            if remaining > 0:
                raise ResendTooSoon(int(remaining) + 1)
            db.execute(
                """INSERT INTO email_challenges(email,purpose,requested_at,delivery_id) VALUES (?,?,?,?)
                       ON CONFLICT(email,purpose) DO UPDATE SET requested_at=excluded.requested_at,delivery_id=excluded.delivery_id""",
                (email, purpose, now, delivery),
            )
        code = f"{secrets.randbelow(1_000_000):06d}"
        digest = password_hash(code)
        try:
            mail.send_code(email, purpose, code)
        except mail.MailUnavailable:
            with self.db.connect() as db:
                db.execute(
                    "UPDATE email_challenges SET requested_at=?,delivery_id=NULL WHERE delivery_id=?",
                    (previous, delivery),
                )
            raise
        with self.db.connect() as db:
            updated = db.execute(
                """UPDATE email_challenges SET code_hash=?,expires_at=?,attempts=?,delivery_id=NULL
                                    WHERE delivery_id=?""",
                (digest, time.time() + CODE_TTL, MAX_ATTEMPTS, delivery),
            )
            if not updated.rowcount:
                raise ValueError("请求已更新，请使用最新验证码")
        return {"expires_in": CODE_TTL, "resend_after": RESEND_AFTER}

    def verified_session(self, purpose, email, password, code):
        email = self.email_address(email)
        if not isinstance(code, str) or not re.fullmatch(r"[0-9]{6}", code):
            raise ValueError("请输入 6 位邮箱验证码")
        if purpose == "register":
            self.registration_password(password)
            hashed = password_hash(password)
        elif purpose == "login":
            user = self.login(email, password)
        else:
            raise ValueError("验证码用途无效")
        with self.db.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            now = time.time()
            row = db.execute(
                """SELECT code_hash,expires_at,attempts FROM email_challenges
                                WHERE email=? AND purpose=?""",
                (email, purpose),
            ).fetchone()
            if (
                not row
                or not row["code_hash"]
                or row["expires_at"] <= now
                or not row["attempts"]
            ):
                raise ValueError("验证码无效或已过期，请重新获取")
            if password_matches(code, row["code_hash"]):
                if purpose == "register":
                    user = self.insert_user(db, email, hashed)
                token, csrf = self.store_session(db, user)
                db.execute(
                    """UPDATE email_challenges SET code_hash=NULL,expires_at=NULL,attempts=0,
                              delivery_id=NULL WHERE email=? AND purpose=?""",
                    (email, purpose),
                )
                return user, token, csrf
            db.execute(
                "UPDATE email_challenges SET attempts=attempts-1 WHERE email=? AND purpose=?",
                (email, purpose),
            )
        raise ValueError("验证码不正确或已失效")

    def store_session(self, db, user):
        token, csrf = secrets.token_urlsafe(32), secrets.token_urlsafe(24)
        db.execute("DELETE FROM sessions WHERE expires_at<?", (time.time(),))
        db.execute(
            "INSERT INTO sessions VALUES (?,?,?,?)",
            (self.db.digest(token), user["id"], csrf, time.time() + 7 * 86400),
        )
        return token, csrf

    def create_session(self, user):
        with self.db.connect() as db:
            return self.store_session(db, user)

    def session(self, token):
        if not token:
            return None
        with self.db.connect() as db:
            row = db.execute("""SELECT users.id,users.email,sessions.csrf FROM sessions
                JOIN users ON users.id=sessions.user_id WHERE token_hash=? AND expires_at>?""",
                             (self.db.digest(token), time.time())).fetchone()
        return dict(row) if row else None

    def logout(self, token):
        with self.db.connect() as db:
            db.execute("DELETE FROM sessions WHERE token_hash=?", (self.db.digest(token or ""),))
