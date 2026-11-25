// Import a previously bound project into a private application database.
package main

import (
	"context"
	"flag"
	"log"
	"memosphere/internal/memory"
	"os"
	"path/filepath"
)

func main() {
	source := flag.String("source", "", "SQLite snapshot of previous service")
	dest := flag.String("db", "", "private application SQLite database")
	subject := flag.String("subject", "", "immutable host account ID")
	project := flag.String("project", "", "previously bound project ID")
	connection := flag.String("connection", "", "previously bound connection ID")
	flag.Parse()
	a, _ := filepath.Abs(*source)
	b, _ := filepath.Abs(*dest)
	if *source == "" || *dest == "" || a == b {
		log.Fatal("需要分别指定旧库副本与目标库")
	}
	src, e := os.Stat(a)
	if e != nil {
		log.Fatal(e)
	}
	if out, e := os.Stat(b); e == nil && os.SameFile(src, out) {
		log.Fatal("源与目标库相同")
	}
	if err := memory.CheckDatabaseMode(b, true); err != nil {
		log.Fatal(err)
	}
	db, e := memory.Open(b)
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	if e = db.ImportApplicationProject(context.Background(), a, *subject, *project, *connection); e != nil {
		log.Fatal(e)
	}
	log.Print("应用记忆项目已就绪")
}
