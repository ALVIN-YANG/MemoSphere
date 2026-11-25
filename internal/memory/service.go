package memory

import (
	"errors"
	"go.opentelemetry.io/otel/trace"
	"io/fs"
	"memosphere/internal/embedding"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Service struct {
	categoryComparison string // Empty for the live policy. Baselines are only set by the isolated benchmark.
	tracer             trace.Tracer
	DB                 DB
	Origin             string
	Vectors            embedding.Encoder
	Lexical            bool
	Policy             Policy
	Model              *Model
	Assets             fs.FS
	dummy              string
	mail               func(string, string, string) error
	mu                 sync.Mutex
	attempts           map[string][]time.Time
}

func New(db DB, origin string, assets fs.FS) (*Service, error) {
	origin = strings.TrimRight(origin, "/")
	u, e := url.Parse(origin)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) {
		return nil, errors.New("invalid public URL")
	}
	return &Service{DB: db, Origin: origin, Assets: assets, Policy: defaultPolicy(), dummy: hashPassword(token(24), ""), mail: smtpMail, attempts: map[string][]time.Time{}}, nil
}
func loopback(host string) bool { return host == "localhost" || net.ParseIP(host).IsLoopback() }
