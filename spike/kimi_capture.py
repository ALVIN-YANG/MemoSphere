"""按授权输入事件启动的有界本地补采集任务，不扫描其他会话。"""

import argparse
import asyncio
import json
import time
from pathlib import Path
from urllib.parse import quote
from urllib.request import Request, urlopen

from spike.connector import handle, kimi_endpoint, kimi_messages, private_json


def wait_for_reply(config, event, timeout=90):
    deadline = time.monotonic() + timeout
    endpoint = kimi_endpoint(config)
    session = quote(event["session_id"], safe="")
    token = Path(config["kimi_token_file"]).read_text().strip()
    while time.monotonic() < deadline:
        time.sleep(1)
        # kimi_messages 负责验证回环地址、当前会话目录及消息来源类型。
        messages = kimi_messages(config, event)
        known = set(event.get("memosphere_known_assistant_ids", []))
        if not any(m["role"] == "assistant" and m["source_id"] not in known for m in messages):
            continue
        req = Request(endpoint + f"/api/v1/sessions/{session}/prompts",
                      headers={"Authorization": "Bearer " + token})
        with urlopen(req, timeout=3) as response:
            value = json.load(response)
        if value.get("code") != 0:
            raise RuntimeError("Kimi 轮次状态读取失败")
        if value["data"].get("active") is None and not value["data"].get("queued"):
            return
    raise TimeoutError("Kimi 轮次尚未完成，保留补采集任务")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--job", type=Path, required=True)
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    event = json.loads(args.job.read_text())
    config["workspaces"] = {str(Path(p).resolve()): v for p, v in config["workspaces"].items()}
    if str(Path(event.get("cwd", "/")).resolve()) not in config["workspaces"]:
        return
    try:
        wait_for_reply(config, event)
        asyncio.run(handle(config, {**event, "hook_event_name": "Stop"}))
        args.job.unlink(missing_ok=True)
        status = {"event": "KimiCapture", "session_id": event["session_id"], "success": True}
    except Exception as error:
        status = {"event": "KimiCapture", "session_id": event["session_id"],
                  "success": False, "error_type": type(error).__name__}
        private_json(args.job.with_suffix(".status.json"), status)
    status["time"] = time.time()
    with Path(config["audit"]).open("a") as f:
        f.write(json.dumps(status) + "\n")


if __name__ == "__main__":
    main()
