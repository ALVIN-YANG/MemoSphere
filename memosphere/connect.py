"""一次性接入码安装器。保留现有 Hook，不修改客户端信任与审批设置。"""

import argparse
import json
import shlex
import sys
import tomllib
from pathlib import Path
from urllib.parse import urlparse
from urllib.request import Request, urlopen

from spike.connector import private_json

ROOT = Path(__file__).resolve().parents[1]
LOOPBACK = {"127.0.0.1", "localhost", "::1"}


def service_url(value):
    value = value.rstrip("/")
    parsed = urlparse(value)
    if (parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username
            or parsed.password or parsed.path or parsed.query or parsed.fragment):
        raise ValueError("服务地址格式无效")
    if parsed.scheme == "http" and parsed.hostname not in LOOPBACK:
        raise ValueError("仅本机回环服务可以使用 HTTP")
    return value


def kimi_instance(home):
    candidates = []
    for path in (home / ".kimi-code/server/instances").glob("*.json"):
        try:
            value = json.loads(path.read_text())
            if value.get("host") in LOOPBACK and isinstance(value.get("port"), int):
                candidates.append(value)
        except (OSError, ValueError):
            continue
    if not candidates:
        raise ValueError("请先启动 Kimi Code 桌面端，再执行接入命令")
    instance = max(candidates, key=lambda v: v.get("heartbeat_at", 0))
    return "http://127.0.0.1:" + str(instance["port"])


def kimi_api(home, body=None):
    token = (home / ".kimi-code/server.token").read_text().strip()
    request = Request(kimi_instance(home) + "/api/v1/config",
                      data=None if body is None else json.dumps(body).encode(),
                      headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    with urlopen(request, timeout=10) as response:
        result = json.load(response)
    if result.get("code") != 0:
        raise ValueError("Kimi 配置接口未接受变更")
    return result["data"]


class Installer:
    def __init__(self, home=None):
        self.home = Path(home) if home else Path.home()
        self.base = self.home / ".memosphere/connections"

    def path(self, connection):
        # ID 来自远端，也必须限制路径，防止写出私有目录。
        import uuid
        return self.base / (str(uuid.UUID(connection)) + ".json")

    @staticmethod
    def command(path):
        return "cd " + shlex.quote(str(ROOT)) + " && " + shlex.join(
            [sys.executable, "-m", "spike.connector", "--config", str(path)])

    def codex_hooks(self, command, remove=False):
        path = self.home / ".codex/hooks.json"
        document = json.loads(path.read_text()) if path.exists() else {}
        hooks = document.setdefault("hooks", {})
        for event in ["UserPromptSubmit", "Stop", "PostToolUse"]:
            groups = hooks.get(event, [])
            retained = []
            for group in groups:
                # 只移除本连接的命令；保留同一组内其他 Hook。
                remaining = [h for h in group.get("hooks", []) if h.get("command") != command]
                if remaining:retained.append({**group, "hooks": remaining})
            if not remove:
                retained.append({"hooks": [{"type": "command", "command": command, "timeout": 15}]})
            if retained:hooks[event] = retained
            else:hooks.pop(event, None)
        private_json(path, document)

    def kimi_hooks(self, command, remove=False):
        # 配置 API 更新正在运行的桌面进程；不重写用户 provider、密钥或其他配置。
        kimi_api(self.home)
        path = self.home / ".kimi-code/config.toml"
        current = tomllib.loads(path.read_text()).get("hooks", [])
        hooks = [h for h in current if h.get("command") != command]
        if not remove:
            hooks.extend({"event": e, "command": command, "timeout": 15}
                         for e in ["UserPromptSubmit", "Stop"])
        kimi_api(self.home, {"hooks": hooks})

    def install(self, grant, workspace, project):
        if grant["kind"] not in {"codex", "kimi"}:
            raise ValueError("其他 MCP 客户端请在控制台更换 Key 后手动配置")
        if project not in grant["projects"]:
            raise ValueError("项目不在此 Agent 的授权范围内")
        workspace = Path(workspace).expanduser().resolve(strict=True)
        if not workspace.is_dir():raise ValueError("工作区必须是目录")
        path = self.path(grant["connection_id"])
        self.base.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.base.parent.chmod(0o700)
        self.base.chmod(0o700)
        config = {"agent": grant["kind"], "url": grant["url"], "key": grant["key"],
                  "policy_url": grant["url"].removesuffix("/mcp/") + "/api/connect/policy",
                  "workspaces": {str(workspace): project},
                  "spool": str(path.with_suffix("") / "spool"),
                  "audit": str(path.with_suffix("") / "audit.jsonl"),
                  "read_access": grant.get("read_access", True),
                  "write_access": grant.get("write_access", True)}
        if config["agent"] == "kimi":
            config.update(kimi_api=kimi_instance(self.home), kimi_home=str(self.home),
                          kimi_token_file=str(self.home / ".kimi-code/server.token"))
        # 即使配置 Hook 失败也保留已兑换的凭证，可通过 --resume 重试。
        private_json(path, config)
        print("已保存本机凭证。连接 ID：", path.stem)
        self.activate(path, config)
        return path

    def activate(self, path, config):
        method = self.codex_hooks if config["agent"] == "codex" else self.kimi_hooks
        method(self.command(path))

    def uninstall(self, connection):
        path = self.path(connection)
        config = json.loads(path.read_text())
        method = self.codex_hooks if config["agent"] == "codex" else self.kimi_hooks
        method(self.command(path), remove=True)
        path.unlink()
        # 原始离线数据不静默删除；控制台撤销才能使远端凭证失效。


def main():
    parser = argparse.ArgumentParser(description="Memory Sphere 桌面接入组件")
    parser.add_argument("--url")
    parser.add_argument("--code")
    parser.add_argument("--workspace", type=Path)
    parser.add_argument("--project")
    parser.add_argument("--resume", metavar="CONNECTION_ID")
    parser.add_argument("--uninstall", metavar="CONNECTION_ID")
    args = parser.parse_args()
    installer = Installer()
    try:
        if args.uninstall:
            installer.uninstall(args.uninstall)
            print("已移除本连接的 Hook 和本机 Key。请在控制台撤销远端授权；离线待提交数据保留。")
            return
        if args.resume:
            path = installer.path(args.resume)
            config = json.loads(path.read_text())
            installer.activate(path, config)
        else:
            if not args.url or not args.code:parser.error("请提供控制台生成的 --url 和 --code")
            base = service_url(args.url)
            workspace = args.workspace or Path(input("绑定的本机项目目录：").strip()).expanduser()
            workspace = workspace.resolve(strict=True)
            if not workspace.is_dir():raise ValueError("工作区必须是目录")
            request = Request(base + "/api/connect/exchange", data=json.dumps({"code": args.code}).encode(),
                              headers={"Content-Type": "application/json"})
            with urlopen(request, timeout=15) as response:grant = json.load(response)
            # 服务端不能指示组件把 Key 发往其他服务。
            if grant["url"] != base + "/mcp/":raise ValueError("服务返回的 MCP 地址不匹配")
            project = args.project
            if project is None:
                if len(grant["projects"]) == 1:project = grant["projects"][0]
                else:
                    print("此连接有多个项目：", ", ".join(grant["projects"]))
                    project = input("绑定的项目 ID：").strip()
            path = installer.install(grant, workspace, project)
            config = json.loads(path.read_text())
        print("已安装限当前工作区生效的 Hook。连接 ID：", path.stem)
        if config["agent"] == "codex":
            print("下一步：在 Codex 设置 → Hooks 检查并信任此连接的三个 Hook，然后打开新会话。")
        else:print("下一步：在 Kimi Code 打开该项目的新会话，再回控制台查看同步结果。")
        print("没有更改客户端的信任状态或审批模式。首次收到消息前，控制台显示等待同步。")
    except Exception as error:
        # urllib 异常可能包含响应内容或 URL；不把凭据写入终端/日志。
        print("接入未完成（" + type(error).__name__ + "）。请检查目录、接入码和客户端运行状态。", file=sys.stderr)
        print("若已兑换接入码，可用 --resume 连接ID 重试 Hook 安装；也可在控制台重新生成接入码。", file=sys.stderr)
        raise SystemExit(1) from None


if __name__ == "__main__":main()
