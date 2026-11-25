package memory

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

func privateJSON(path string, target any) error {
	fi, e := os.Stat(path)
	if e != nil {
		return e
	}
	if fi.Mode().Perm()&0077 != 0 {
		return errors.New("private configuration requires mode 0600")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, target)
}
func smtpMail(email, purpose, code string) error {
	d := Row{}
	path := os.Getenv("MEMOSPHERE_SMTP_CONFIG")
	if path == "" {
		if _, e := os.Stat(".runtime/smtp.json"); e == nil {
			path = ".runtime/smtp.json"
		}
	}
	if path != "" {
		if e := privateJSON(path, &d); e != nil {
			return e
		}
	}
	get := func(k, def string) string {
		if v, ok := os.LookupEnv("MEMOSPHERE_SMTP_" + strings.ToUpper(k)); ok {
			return v
		}
		if v := d[k]; v != nil {
			return fmt.Sprint(v)
		}
		return def
	}
	host := get("host", "")
	security := get("security", "starttls")
	sender := get("from", "")
	from, e := mail.ParseAddress(sender)
	if e != nil || host == "" || strings.ContainsAny(sender, "\r\n") {
		return errors.New("invalid SMTP configuration")
	}
	port := 587
	if security == "tls" {
		port = 465
	}
	addr := net.JoinHostPort(host, get("port", strconv.Itoa(port)))
	conf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	dial := net.Dialer{Timeout: 12 * time.Second}
	var conn net.Conn
	switch security {
	case "tls":
		conn, e = tls.DialWithDialer(&dial, "tcp", addr, conf)
	case "starttls", "plain":
		if security == "plain" && !loopback(host) {
			return errors.New("unencrypted SMTP")
		}
		conn, e = dial.Dial("tcp", addr)
	default:
		return errors.New("invalid SMTP security")
	}
	if e != nil {
		return e
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(12 * time.Second))
	c, e := smtp.NewClient(conn, host)
	if e != nil {
		return e
	}
	defer c.Close()
	if security == "starttls" {
		if e = c.StartTLS(conf); e != nil {
			return e
		}
	}
	user, pass := get("user", ""), get("password", "")
	if (user == "") != (pass == "") {
		return errors.New("invalid SMTP credentials")
	}
	if user != "" {
		if e = c.Auth(smtp.PlainAuth("", user, pass, host)); e != nil {
			return e
		}
	}
	if e = c.Mail(from.Address); e != nil {
		return e
	}
	if e = c.Rcpt(email); e != nil {
		return e
	}
	w, e := c.Data()
	if e != nil {
		return e
	}
	label := map[string]string{"login": "登录", "register": "注册"}[purpose]
	_, e = fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n你的%s验证码是 %s，10 分钟内有效。\r\n", from.String(), email, mime.BEncoding.Encode("utf-8", "MemoSphere "+label+"验证码"), label, code)
	if e != nil {
		return e
	}
	return w.Close()
}
