"""模型输入与记忆输出的确定性保护；不把识别结果当作完整敏感分类器。"""

import re

KEY = re.compile(r"\b(?:(?:sk-|ms_)[A-Za-z0-9_-]{12,}|Bearer\s+[A-Za-z0-9._-]{16,})", re.I)
ASSIGNMENT = re.compile(r'''((?:api[_ -]?key|access[_ -]?token|password|密码|密钥)["']?\s*(?:[:：=]|是|为)\s*["']?)([^\s，。；;"'，}]+)''', re.I)
PEM = re.compile(r"-----BEGIN [^-]*PRIVATE KEY-----[\s\S]*?-----END [^-]*PRIVATE KEY-----")
PHONE = re.compile(r"(?<!\d)1[3-9]\d{9}(?!\d)")


def redact(text):
    text = PEM.sub("[已屏蔽凭据]", text)
    text = KEY.sub("[已屏蔽凭据]", text)
    return ASSIGNMENT.sub(lambda m: m[1] + "[已屏蔽凭据]", text)


def contains_secret(text):
    return redact(text) != text or "[已屏蔽凭据]" in text


def sensitive_text(text):
    return bool(PHONE.search(text))
