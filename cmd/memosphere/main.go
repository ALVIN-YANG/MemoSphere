// Command memosphere serves the owner console, HTTP API and MCP from one process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"memosphere/internal/embedding"
	"memosphere/internal/memory"
	assets "memosphere/memosphere"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func env(k, fallback string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return fallback
}
func run() error {
	appMode := flag.Bool("application", false, "serve the private host-application API instead of the standalone console")
	path := flag.String("db", env("MEMOSPHERE_DB", ".runtime/app.sqlite3"), "SQLite database path")
	host := flag.String("host", "127.0.0.1", "bind address")
	port := flag.Int("port", 18800, "listen port")
	origin := flag.String("public-url", env("MEMOSPHERE_PUBLIC_URL", ""), "owner console origin")
	model := flag.String("model-config", env("MEMOSPHERE_MODEL_CONFIG", ".runtime/model.json"), "private model settings")
	policy := flag.String("conversation-policy", env("MEMOSPHERE_CONVERSATION_POLICY", ""), "conversation storage policy")
	reset := flag.Bool("reset-vector-index", false, "probe the configured encoder, clear vector rows in the stopped database, and exit")
	flag.Parse()
	if *appMode && os.Getenv("MEMOSPHERE_DB") == "" {
		explicitDB := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "db" {
				explicitDB = true
			}
		})
		if !explicitDB {
			*path = ".runtime/application.sqlite3"
		}
	}
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	if *origin == "" {
		*origin = "http://" + addr
	}
	if err := memory.CheckDatabaseMode(*path, *appMode); err != nil {
		return err
	}
	db, e := memory.Open(*path)
	if e != nil {
		return e
	}
	defer db.Close()
	s, e := memory.New(db, *origin, assets.Console())
	if e != nil {
		return e
	}
	if e = s.LoadPolicy(*policy); e != nil {
		return e
	}
	s.Model, e = memory.LoadModel(*model)
	if e != nil {
		return e
	}
	mode := env("MEMOSPHERE_RETRIEVAL", "hybrid")
	if mode != "hybrid" && mode != "lexical" {
		return errors.New("invalid retrieval mode")
	}
	s.Lexical = mode == "lexical"
	if !s.Lexical {
		switch env("MEMOSPHERE_EMBEDDING_PROVIDER", "bge") {
		case "bge":
			local, err := embedding.Open(env("MEMOSPHERE_EMBEDDING_DIR", ".runtime/bge-small-zh-v1.5"), os.Getenv("MEMOSPHERE_ONNX_LIBRARY"))
			if err != nil {
				log.Print("semantic retrieval unavailable")
			} else {
				s.Vectors = local
				defer local.Close()
			}
		case "dashscope":
			remote, err := embedding.NewDashScope(os.Getenv("DASHSCOPE_WORKSPACE_ID"), os.Getenv("DASHSCOPE_API_KEY"))
			if err != nil {
				return err
			}
			s.Vectors = remote
		default:
			return errors.New("unknown embedding provider")
		}
	}
	stopTracing := s.ConfigureTracing()
	defer stopTracing()
	var handler http.Handler
	if *appMode {
		handler, e = s.ApplicationHandler(os.Getenv("MEMOSPHERE_APP_TOKEN"))
		if e != nil {
			return e
		}
	} else {
		application, err := db.IsApplicationDB()
		if err != nil {
			return err
		}
		if application {
			return errors.New("此库属于应用模式，请使用 --application")
		}
		handler = s.Handler()
	}
	if err := s.PrepareVectorIndex(context.Background(), *reset); err != nil {
		return err
	}
	if *reset {
		log.Print("向量表已清空")
		return nil
	}

	server := http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	listener, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	wait := s.Workers(ctx)
	defer func() { cancel(); wait() }()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	log.Printf("MemoSphere listening on %s", addr)
	select {
	case <-ctx.Done():
		shutdown, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		return server.Shutdown(shutdown)
	case e := <-stopped:
		return e
	}
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
