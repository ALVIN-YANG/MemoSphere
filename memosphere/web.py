"""账号控制台与同源 API，MCP 使用独立的 Agent Key，不接受浏览器会话替代。"""

import argparse
import asyncio
import hmac
import json
import os
import shlex
import time
from contextlib import asynccontextmanager
from pathlib import Path
from urllib.parse import urlparse

import uvicorn
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import FileResponse, JSONResponse
from starlette.routing import Mount, Route
from starlette.staticfiles import StaticFiles

from memosphere.accounts import Accounts, ResendTooSoon
from memosphere.mail import MailUnavailable
from memosphere.agents import Agents
from memosphere.database import Database
from memosphere.mcp import create_mcp
from memosphere.telemetry import memory_tracing
from memosphere.spaces import Spaces
from memosphere.extraction import MemoryEngine
from memosphere.model import JsonModel, ModelConfig
from spike.server import bearer

ASSETS = Path(__file__).parent / "static"


class APIError(Exception):
    def __init__(self, message, status=400):
        self.message, self.status = message, status


def create_app(db_path=".runtime/app.sqlite3", public_url="http://127.0.0.1:18800", model=None):
    public_url = public_url.rstrip("/")
    parsed = urlparse(public_url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.query or parsed.fragment or parsed.path:
        raise ValueError("服务地址必须是不带路径和凭证的 HTTP/HTTPS 地址")
    if parsed.scheme == "http" and parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
        raise ValueError("公网服务必须使用 HTTPS")
    database = Database(db_path)
    from memosphere.embedding import VectorIndex
    database.retrieval_mode = os.environ.get("MEMOSPHERE_RETRIEVAL", "hybrid")
    if database.retrieval_mode not in {"hybrid", "lexical"}:
        raise ValueError("无效检索模式")
    database.vector_index = VectorIndex(database, os.environ.get("MEMOSPHERE_EMBEDDING_DIR", ".runtime/bge-small-zh-v1.5")) if database.retrieval_mode == "hybrid" else None
    accounts, spaces = Accounts(database), Spaces(database)
    agents = Agents(database, spaces)
    engine = MemoryEngine(database, spaces, model) if model else None
    mcp_app, mcp_server = create_mcp(database, spaces, public_url)
    attempts = {}

    def rate_limit(request, scope, limit=20):
        now = time.monotonic()
        key = (request.client.host if request.client else "local", scope)
        entries = [t for t in attempts.get(key, []) if now - t < 60]
        if len(entries) >= limit:raise APIError("操作太频繁，请稍后重试", 429)
        attempts[key] = [*entries, now]
        if len(attempts) > 10000:
            for k in list(attempts):
                if not attempts[k] or now - attempts[k][-1] >= 60:attempts.pop(k, None)

    def origin(request):
        if request.headers.get("origin") != public_url:
            raise APIError("请求来源不符合当前服务地址，请从控制台重新打开", 403)

    def user(request, write=False):
        current = accounts.session(request.cookies.get("ms_session"))
        if not current:raise APIError("请先登录", 401)
        if write:
            origin(request)
            if not hmac.compare_digest(request.headers.get("x-csrf-token", ""), current["csrf"]):
                raise APIError("页面凭证已失效，请刷新后重试", 403)
        return current

    async def body(request):
        chunks, size = [], 0
        async for chunk in request.stream():
            size += len(chunk)
            if size > 65536:raise APIError("提交内容过大", 413)
            chunks.append(chunk)
        try:
            value = json.loads(b"".join(chunks))
            if not isinstance(value, dict):raise ValueError()
            return value
        except (ValueError, UnicodeDecodeError):raise APIError("提交的数据格式无效") from None

    def signed_in(current, token, csrf):
        response = JSONResponse({"user": current, "csrf": csrf})
        response.set_cookie("ms_session", token, max_age=7 * 86400, httponly=True,
                            secure=parsed.scheme == "https", samesite="lax", path="/")
        return response

    async def send_email_code(request):
        origin(request);rate_limit(request, "mail", 10)
        data = await body(request)
        try:
            result = await asyncio.to_thread(accounts.request_code, data.get("purpose"), data.get("email"), data.get("password"))
        except ResendTooSoon as error:
            return JSONResponse({"error":str(error), "resend_after":error.retry_after}, status_code=429, headers={"Retry-After":str(error.retry_after)})
        except MailUnavailable as error:
            raise APIError(str(error), 503) from None
        return JSONResponse(result)

    async def authenticate(request):
        origin(request);rate_limit(request, "auth")
        data = await body(request)
        result = await asyncio.to_thread(accounts.verified_session, request.url.path.rsplit("/",1)[-1],
                                         data.get("email"), data.get("password"), data.get("code"))
        return signed_in(*result)

    async def hosted_grant(request):
        from memosphere.hosted import grant
        current = user(request, True)
        data = await body(request)
        if set(data) != {"consent"} or data["consent"] is not True:
            raise ValueError("请确认允许 KoKo 读写专属项目")
        binding = grant(database, current["id"], request.headers.get("x-hosted-client", ""))
        return JSONResponse(binding)

    async def logout(request):
        user(request, True)
        accounts.logout(request.cookies.get("ms_session"))
        response = JSONResponse({"ok": True});response.delete_cookie("ms_session", path="/")
        return response

    async def session(request):
        current = user(request)
        with database.connect() as db:
            counts = {name: db.execute(f"SELECT count(*) FROM {table} WHERE user_id=?" + extra,
                       (current["id"],)).fetchone()[0] for name, table, extra in [
                ("memories", "memories", ""), ("projects", "projects", ""),
                ("agents", "connections", " AND revoked_at IS NULL"), ("messages", "messages", "")]}
            processing = {r["state"]: r["n"] for r in db.execute("""SELECT j.state,count(*) AS n FROM extraction_jobs j
                JOIN messages m ON m.id=j.message_id WHERE m.user_id=? GROUP BY j.state""", (current["id"],))}
            usage = dict(db.execute("""SELECT count(*) AS calls,coalesce(sum(input_tokens),0) AS input_tokens,
                coalesce(sum(output_tokens),0) AS output_tokens FROM model_usage u JOIN messages m ON m.id=u.message_id
                WHERE m.user_id=?""", (current["id"],)).fetchone())
            epoch = db.execute("SELECT epoch FROM memory_epochs WHERE user_id=?", (current["id"],)).fetchone()
        return JSONResponse({"user": {"id": current["id"], "email": current["email"]}, "csrf": current["csrf"],
                             "counts": counts, "public_url": public_url, "automatic_extraction": engine is not None,
                             "processing": processing, "model_usage": usage, "memory_epoch": epoch[0] if epoch else 0})

    async def projects(request):
        current = user(request, request.method == "POST")
        if request.method == "POST":
            return JSONResponse(spaces.create_project(current["id"], (await body(request)).get("name", "")), status_code=201)
        return JSONResponse({"items": spaces.projects(current["id"])})

    async def memories(request):
        current = user(request, request.method == "POST")
        if request.method == "POST":return JSONResponse(spaces.save(current["id"], await body(request)), status_code=201)
        query = request.query_params
        try:offset = int(query.get("offset", "0"))
        except ValueError:raise APIError("分页参数无效") from None
        return JSONResponse(spaces.memories(current["id"], query.get("q", "")[:500], query.get("project"),
                                           True if query.get("sensitive") == "1" else None, offset, query.get("state", "active"),
                                           category=query.get("category"), entity=query.get("entity"), order=query.get("order", "updated")))

    async def memory(request):
        current = user(request, request.method != "GET")
        memory_id = request.path_params["memory_id"]
        if request.method == "GET":return JSONResponse(spaces.memory(current["id"], memory_id))
        if request.method == "PUT":return JSONResponse(spaces.save(current["id"], await body(request), memory_id))
        deleted = spaces.delete(current["id"], memory_id)
        return JSONResponse({"ok": True, "deleted": deleted})

    async def connections(request):
        current = user(request, request.method == "POST")
        if request.method == "POST":return JSONResponse(agents.create(current["id"], await body(request)), status_code=201)
        return JSONResponse({"items": agents.list(current["id"])})

    async def connection(request):
        current = user(request, True);connection_id = request.path_params["connection_id"]
        if request.method == "PATCH":agents.update(current["id"], connection_id, await body(request))
        else:agents.revoke(current["id"], connection_id)
        return JSONResponse({"ok": True})

    async def setup(request):
        current = user(request, True)
        connection_id = request.path_params["connection_id"]
        code = agents.setup_code(current["id"], connection_id)
        with database.connect() as db:
            kind = agents.owned(db, current["id"], connection_id)["kind"]
        if kind == "other":
            return JSONResponse({"code":code, "expires_in":600, "url":public_url+"/mcp/",
                                 "exchange_url":public_url+"/api/connect/exchange"})
        command = shlex.join(["uv", "run", "python", "-m", "memosphere.connect", "--url", public_url, "--code", code])
        return JSONResponse({"code": code, "expires_in": 600, "command": command})

    async def rotate(request):
        current = user(request, True)
        return JSONResponse({"key": agents.rotate(current["id"], request.path_params["connection_id"])})

    async def exchange(request):
        rate_limit(request, "pairing", 30)
        data = await body(request)
        code = data.get("code", "")
        if not isinstance(code, str) or len(code) > 128:raise APIError("接入码格式无效")
        result = agents.exchange(code)
        result["url"] = public_url + "/mcp/"
        return JSONResponse(result)

    async def connection_policy(request):
        agent = database.authenticate(bearer(request.headers))
        return JSONResponse({"projects": json.loads(agent["projects"]),
            **{k: bool(agent[k]) for k in ["global_access", "sensitive_access", "read_access", "write_access"]}})

    async def activity(request):
        current = user(request)
        with database.connect() as db:
            rows = db.execute("SELECT id,action,subject,details,created_at FROM events WHERE user_id=? ORDER BY id DESC LIMIT 100", (current["id"],))
            return JSONResponse({"items": [{**dict(r), "details": json.loads(r["details"])} for r in rows]})

    async def conversations(request):
        current = user(request)
        with database.connect() as db:
            rows = db.execute("""SELECT m.id,m.agent_id,m.session_id,m.project_id,m.role,m.text,m.recorded_at,
                c.name AS agent_name,j.state AS processing_state,j.error AS processing_error,j.result AS processing_result
                FROM messages m LEFT JOIN connections c ON c.id=m.agent_id LEFT JOIN extraction_jobs j ON j.message_id=m.id
                WHERE m.user_id=? ORDER BY m.id DESC LIMIT 100""", (current["id"],))
            return JSONResponse({"items": [dict(r) for r in rows]})

    async def export(request):
        current = user(request)
        # 一致的读快照，导出期间发生编辑/删除也不会漏项或混入其他版本。
        with database.connect() as db:
            db.execute("BEGIN")
            all_memories = []
            for row in db.execute("SELECT * FROM memories WHERE user_id=? ORDER BY id", (current["id"],)):
                item = spaces.decode(row)
                item["versions"] = [{**dict(v), "content": json.loads(v["content"])} for v in db.execute(
                    "SELECT * FROM memory_versions WHERE memory_id=? ORDER BY revision DESC", (item["id"],))]
                item["sources"] = [dict(r) for r in db.execute("SELECT revision,message_id,quote FROM memory_sources WHERE memory_id=?", (item["id"],))]
                all_memories.append(item)
            projects = [dict(r) for r in db.execute("SELECT * FROM projects WHERE user_id=?", (current["id"],))]
            conversations = [dict(r) for r in db.execute("SELECT * FROM messages WHERE user_id=? ORDER BY id", (current["id"],))]
        return JSONResponse({"format": "memosphere-v1", "exported_at": time.time(),
                             "projects": projects, "memories": all_memories, "conversations": conversations},
                            headers={"Content-Disposition": 'attachment; filename="memosphere-memories.json"'})

    async def index(request):return FileResponse(ASSETS / "index.html", media_type="text/html")
    async def health(request):return JSONResponse({"status": "ok", "version": "0.3.0"})

    async def errors(request, exception):
        if isinstance(exception, APIError):return JSONResponse({"error": exception.message}, status_code=exception.status)
        if isinstance(exception, LookupError):return JSONResponse({"error": "内容不存在或无权访问"}, status_code=404)
        if isinstance(exception, PermissionError):return JSONResponse({"error": str(exception)}, status_code=403)
        return JSONResponse({"error": str(exception)}, status_code=400)

    @asynccontextmanager
    async def lifespan(app):
        from memosphere.embedding import indexing
        async with memory_tracing(database), mcp_server.session_manager.run(), indexing(database.vector_index):
            task = asyncio.create_task(engine.run()) if engine else None
            try:yield
            finally:
                if task:
                    task.cancel()
                    try:await task
                    except asyncio.CancelledError:pass

    app = Starlette(routes=[
        Route("/", index), Route("/health", health),
        Route("/api/hosted/grant", hosted_grant, methods=["POST"]),
        Route("/api/auth/code", send_email_code, methods=["POST"]),
        Route("/api/auth/register", authenticate, methods=["POST"]), Route("/api/auth/login", authenticate, methods=["POST"]),
        Route("/api/auth/logout", logout, methods=["POST"]), Route("/api/session", session),
        Route("/api/projects", projects, methods=["GET", "POST"]),
        Route("/api/memories", memories, methods=["GET", "POST"]),
        Route("/api/memories/{memory_id}", memory, methods=["GET", "PUT", "DELETE"]),
        Route("/api/agents", connections, methods=["GET", "POST"]),
        Route("/api/agents/{connection_id}", connection, methods=["PATCH", "DELETE"]),
        Route("/api/agents/{connection_id}/setup", setup, methods=["POST"]),
        Route("/api/agents/{connection_id}/rotate", rotate, methods=["POST"]),
        Route("/api/connect/exchange", exchange, methods=["POST"]),
        Route("/api/connect/policy", connection_policy),
        Route("/api/activity", activity), Route("/api/conversations", conversations), Route("/api/export", export),
        Mount("/mcp", app=mcp_app), Mount("/assets", app=StaticFiles(directory=ASSETS)),
    ], lifespan=lifespan, exception_handlers={kind: errors for kind in [APIError, ValueError, PermissionError, LookupError]})
    app.state.database, app.state.accounts, app.state.agents, app.state.spaces, app.state.engine = database, accounts, agents, spaces, engine

    class SecurityHeaders:
        async def __call__(self, scope, receive, send):
            if scope["type"] == "http":
                headers = dict(scope["headers"])
                if headers.get(b"host", b"").decode() != parsed.netloc:
                    await JSONResponse({"error": "服务地址不匹配"}, status_code=400)(scope, receive, send)
                    return
            async def secure_send(message):
                if message["type"] == "http.response.start":
                    message.setdefault("headers", []).extend([
                        (b"x-content-type-options", b"nosniff"), (b"referrer-policy", b"no-referrer"),
                        (b"x-frame-options", b"DENY"), (b"cache-control", b"no-store"),
                        (b"content-security-policy", b"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"),
                    ])
                await send(message)
            await app(scope, receive, secure_send)

    wrapper = SecurityHeaders()
    wrapper.state = app.state
    return wrapper


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--db", default=os.environ.get("MEMOSPHERE_DB", ".runtime/app.sqlite3"))
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18800)
    parser.add_argument("--public-url", default=os.environ.get("MEMOSPHERE_PUBLIC_URL"))
    parser.add_argument("--model-config", default=os.environ.get("MEMOSPHERE_MODEL_CONFIG"), help="平台模型的私有 JSON 配置路径")
    args = parser.parse_args()
    url = args.public_url or f"http://127.0.0.1:{args.port}"
    if args.host not in {"127.0.0.1", "localhost", "::1"} and not url.startswith("https://"):
        parser.error("对外监听前请配置 HTTPS 的 --public-url")
    config = ModelConfig.load(args.model_config) if args.model_config else None
    if args.model_config and config is None:parser.error("找不到平台模型配置，请检查 --model-config 路径")
    uvicorn.run(create_app(args.db, url, JsonModel(config) if config else None), host=args.host, port=args.port, access_log=False, log_level="warning")


if __name__ == "__main__":main()
