package memory

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/google/uuid"
	"golang.org/x/crypto/scrypt"
	"math"
	"math/big"
	"net/mail"
	"regexp"
	"strings"
)

func hexToken(n int) string {
	b := make([]byte, n)
	_, e := rand.Read(b)
	must(e)
	return hex.EncodeToString(b)
}
func hashPassword(s, salt string) string {
	if salt == "" {
		salt = hexToken(16)
	}
	b, e := hex.DecodeString(salt)
	must(e)
	h, e := scrypt.Key([]byte(s), b, 32768, 8, 1, 32)
	must(e)
	return "scrypt$" + salt + "$" + hex.EncodeToString(h)
}
func matches(s, hash string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 3 || len(p[1]) != 32 || p[0] != "scrypt" {
		return false
	}
	if _, e := hex.DecodeString(p[1]); e != nil {
		return false
	}
	return hmac.Equal([]byte(hashPassword(s, p[1])), []byte(hash))
}

var emailFormat = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
var sixDigits = regexp.MustCompile(`^[0-9]{6}$`)

func emailAddress(v any) string {
	s := strings.ToLower(strings.TrimSpace(str(v)))
	a, e := mail.ParseAddress(s)
	check(e == nil && a.Address == s && len(s) <= 254 && emailFormat.MatchString(s), "请输入有效的邮箱地址")
	return s
}
func (s *Service) credentials(ctx context.Context, d Row, purpose string) (string, string, Row) {
	email := emailAddress(d["email"])
	password, ok := d["password"].(string)
	check(purpose == "login" || purpose == "register", "验证码用途无效")
	if purpose == "register" {
		check(ok && size(password) >= 10 && size(password) <= 256, "密码需为 10 至 256 个字符")
		return email, password, nil
	}
	check(ok && size(password) <= 256, "邮箱或密码不正确")
	user := s.DB.transaction(ctx, false, func(q tx) any { return q.one("SELECT id,email,password_hash FROM users WHERE email=?", email) }).(Row)
	h := s.dummy
	if user != nil {
		h = str(user["password_hash"])
	}
	good := matches(password, h)
	check(good && user != nil, "邮箱或密码不正确")
	delete(user, "password_hash")
	return email, password, user
}

type cooldown struct{ seconds int }

func (s *Service) sendCode(ctx context.Context, d Row) Row {
	purpose := str(d["purpose"])
	email, _, _ := s.credentials(ctx, d, purpose)
	delivery := hexToken(16)
	previous := s.DB.transaction(ctx, true, func(q tx) any {
		if purpose == "register" {
			check(q.one("SELECT id FROM users WHERE email=?", email) == nil, "该邮箱已注册，请登录")
		}
		n := now()
		q.exec("DELETE FROM email_challenges WHERE requested_at<? AND coalesce(expires_at,0)<=?", n-600, n)
		row := q.one("SELECT requested_at FROM email_challenges WHERE email=? AND purpose=?", email, purpose)
		old, _ := row["requested_at"].(float64)
		if delay := old + 60 - n; delay > 0 {
			panic(cooldown{int(math.Ceil(delay))})
		}
		q.exec("INSERT INTO email_challenges(email,purpose,requested_at,delivery_id) VALUES(?,?,?,?) ON CONFLICT(email,purpose) DO UPDATE SET requested_at=excluded.requested_at,delivery_id=excluded.delivery_id", email, purpose, n, delivery)
		return old
	}).(float64)
	number, e := rand.Int(rand.Reader, big.NewInt(1000000))
	must(e)
	code := fmt.Sprintf("%06d", number)
	hashed := hashPassword(code, "")
	if e = s.mail(email, purpose, code); e != nil {
		s.DB.transaction(context.Background(), true, func(q tx) any {
			q.exec("UPDATE email_challenges SET requested_at=?,delivery_id=NULL WHERE email=? AND purpose=? AND delivery_id=?", previous, email, purpose, delivery)
			return nil
		})
		fail(503, "验证邮件未能发送，请稍后重试或联系服务管理员")
	}
	s.DB.transaction(context.Background(), true, func(q tx) any {
		n, e := q.exec("UPDATE email_challenges SET code_hash=?,expires_at=?,attempts=5,delivery_id=NULL WHERE email=? AND purpose=? AND delivery_id=?", hashed, now()+600, email, purpose, delivery).RowsAffected()
		must(e)
		check(n == 1, "请使用最新验证码")
		return nil
	})
	return Row{"expires_in": 600, "resend_after": 60}
}
func (s *Service) authenticate(ctx context.Context, d Row, purpose string) (Row, string) {
	code := str(d["code"])
	check(sixDigits.MatchString(code), "请输入 6 位邮箱验证码")
	email, password, user := s.credentials(ctx, d, purpose)
	hashed := ""
	if purpose == "register" {
		hashed = hashPassword(password, "")
	}
	key := token(32)
	res := s.DB.transaction(ctx, true, func(q tx) any {
		row := q.one("SELECT * FROM email_challenges WHERE email=? AND purpose=? AND expires_at>? AND attempts>0 AND code_hash IS NOT NULL", email, purpose, now())
		check(row != nil, "验证码无效或已过期，请重新获取")
		if !matches(code, str(row["code_hash"])) {
			q.exec("UPDATE email_challenges SET attempts=attempts-1 WHERE email=? AND purpose=?", email, purpose)
			return nil
		}
		if purpose == "register" {
			id := uuid.NewString()
			check(q.one("SELECT id FROM users WHERE email=?", email) == nil, "该邮箱已注册，请登录")
			q.exec("INSERT INTO users VALUES(?,?,?,?)", id, email, hashed, now())
			user = Row{"id": id, "email": email}
			q.event(id, "account.created", "", nil)
		}
		csrf := token(24)
		q.exec("DELETE FROM sessions WHERE expires_at<?", now())
		q.exec("INSERT INTO sessions VALUES(?,?,?,?)", digest(key), user["id"], csrf, now()+7*86400)
		q.exec("UPDATE email_challenges SET code_hash=NULL,expires_at=NULL,attempts=0,delivery_id=NULL WHERE email=? AND purpose=?", email, purpose)
		return Row{"user": user, "csrf": csrf}
	})
	check(res != nil, "验证码不正确或已失效")
	return res.(Row), key
}
