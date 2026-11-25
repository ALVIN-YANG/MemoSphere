"""Deliver verification mail to a configured SMTP relay."""

import json
import os
import smtplib
import ssl
from email.message import EmailMessage
from pathlib import Path


class MailUnavailable(Exception):
    pass


def send_code(email, purpose, code):
    path = os.environ.get("MEMOSPHERE_SMTP_CONFIG")
    if path is None and Path(".runtime/smtp.json").is_file():
        path = ".runtime/smtp.json"
    try:
        settings = {}
        if path:
            file = Path(path)
            if file.stat().st_mode & 0o077:
                raise MailUnavailable("发信配置文件权限应为 600")
            settings = json.loads(file.read_text())
            if not isinstance(settings, dict):
                raise ValueError("invalid SMTP configuration")

        def setting(name, default=""):
            return os.environ.get(
                "MEMOSPHERE_SMTP_" + name.upper(), settings.get(name, default)
            )

        host, sender = setting("host"), setting("from")
        if not isinstance(host, str) or not isinstance(sender, str) or not host or not sender:
            raise MailUnavailable("尚未配置发信邮箱，请联系服务管理员")
        security = setting("security", "starttls")
        if security not in {"tls", "starttls", "plain"} or (
            security == "plain" and host not in {"127.0.0.1", "::1", "localhost"}
        ):
            raise MailUnavailable("远程邮件服务需要加密连接")
        port = int(setting("port", 465 if security == "tls" else 587))
        user, password = setting("user"), setting("password")
        if not isinstance(user, str) or not isinstance(password, str) or bool(user) != bool(password):
            raise MailUnavailable("邮件服务认证配置不完整")
        label = {"register": "注册", "login": "登录"}[purpose]
        message = EmailMessage()
        message["From"], message["To"] = sender, email
        message["Subject"] = f"MemoSphere {label}验证码"
        message.set_content(
            f"你的{label}验证码是 {code}，10 分钟内有效。\n如果不是你发起的请求，可以忽略这封邮件。"
        )
        context = ssl.create_default_context()
        client = (
            smtplib.SMTP_SSL(host, port, timeout=12, context=context)
            if security == "tls"
            else smtplib.SMTP(host, port, timeout=12)
        )
        with client:
            if security == "starttls":
                client.starttls(context=context)
            if user:
                client.login(user, password)
            client.send_message(message, to_addrs=[email])
    except (OSError, ValueError, TypeError, smtplib.SMTPException):
        raise MailUnavailable("验证邮件未能发送，请稍后重试或联系服务管理员") from None
