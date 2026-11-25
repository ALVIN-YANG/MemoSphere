package memory

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplicationMissingImportAndSecret(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "app.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	core, e := New(db, "http://localhost", nil)
	if e != nil {
		t.Fatal(e)
	}
	secret := strings.Repeat("x", 40)
	h, e := core.ApplicationHandler(secret)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		token, prior string
		want         int
	}{{"", "", 401}, {secret, "missing", 409}, {secret, "", 200}, {secret, "wrong", 409}} {
		req := httptest.NewRequest(http.MethodGet, "/v1/app/projects", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("X-Memory-Subject", "host-user")
		req.Header.Set("X-Memory-Prior-Project", tc.prior)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("got %d want %d", w.Code, tc.want)
		}
	}
}
