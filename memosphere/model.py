"""平台侧统一模型配置与 JSON 调用，不向普通用户暴露模型或密钥设置。"""

import json
import os
import time
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import urlparse

import httpx2


@dataclass(frozen=True)
class ModelConfig:
    base_url: str
    model: str
    api_key: str = field(repr=False)
    thinking: str = "disabled"
    max_tokens: int = 6144
    timeout: float = 90

    @classmethod
    def load(cls, path=None):
        path = Path(path or os.environ.get("MEMOSPHERE_MODEL_CONFIG", ".runtime/model.json"))
        if not path.exists():return None
        if path.stat().st_mode & 0o077:raise ValueError("模型配置文件权限必须为 600")
        data = json.loads(path.read_text())
        parsed = urlparse(data.get("base_url", ""))
        if (parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username
                or parsed.query or parsed.fragment or (parsed.scheme == "http" and parsed.hostname not in {"127.0.0.1", "localhost", "::1"})):
            raise ValueError("模型服务必须使用 HTTPS，本机测试除外")
        if not data.get("api_key") or not data.get("model"):raise ValueError("缺少平台模型配置")
        if data.get("thinking", "disabled") not in {"disabled", "enabled"}:raise ValueError("思考模式配置无效")
        return cls(**data)


class ModelFailure(Exception):
    def __init__(self, code, usage=None):
        super().__init__(code)
        self.code, self.usage = code, usage


class JsonModel:
    def __init__(self, config):
        self.config = config

    async def complete(self, system, payload):
        started = time.monotonic()
        try:
            async with httpx2.AsyncClient(timeout=self.config.timeout, trust_env=False) as client:
                response = await client.post(self.config.base_url.rstrip("/") + "/chat/completions",
                    headers={"Authorization": "Bearer " + self.config.api_key}, json={
                        "model": self.config.model, "messages": [{"role": "system", "content": system},
                            {"role": "user", "content": json.dumps(payload, ensure_ascii=False)}],
                        "response_format": {"type": "json_object"}, "max_tokens": self.config.max_tokens,
                        "thinking": {"type": self.config.thinking}, "stream": False})
        except httpx2.HTTPError:
            raise ModelFailure("model_unreachable") from None
        if response.status_code != 200:
            raise ModelFailure("model_http_" + str(response.status_code))
        usage = {"model": self.config.model, "latency": round(time.monotonic() - started, 4),
                 "input_tokens": 0, "output_tokens": 0, "reasoning_tokens": 0}
        try:
            data = response.json()
            tokens = data.get("usage", {})
            for key, value in [("input_tokens", tokens.get("prompt_tokens", 0)),
                               ("output_tokens", tokens.get("completion_tokens", 0)),
                               ("reasoning_tokens", (tokens.get("completion_tokens_details") or {}).get("reasoning_tokens", 0))]:
                usage[key] = max(0, int(value))
            choice = data["choices"][0]
            if choice.get("finish_reason") != "stop":raise ModelFailure("model_incomplete", usage)
            content = choice["message"]["content"]
            if not isinstance(content, str) or len(content) > 100000:raise ValueError()
            result = json.loads(content)
            if not isinstance(result, dict):raise ValueError()
        except (ValueError, KeyError, TypeError, IndexError):
            raise ModelFailure("model_invalid_json", usage) from None
        return result, usage
