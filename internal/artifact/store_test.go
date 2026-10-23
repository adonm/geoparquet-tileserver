package artifact

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStore(t *testing.T) {
	objects := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/derived/")
		switch r.Method {
		case http.MethodPut:
			if _, ok := objects[key]; ok {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			b, _ := io.ReadAll(r.Body)
			objects[key] = string(b)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			body, ok := objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		}
	}))
	defer srv.Close()

	s, err := New(srv.URL, "derived")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "v1/places.json.gz", "application/gzip", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "v1/places.json.gz", "application/gzip", []byte("other")); err != nil {
		t.Fatal(err)
	}
	b, ok, err := s.Get(context.Background(), "v1/places.json.gz")
	if err != nil || !ok || string(b) != "hello" {
		t.Fatalf("get = %q %v %v", b, ok, err)
	}
	if _, ok, err := s.Get(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("missing = %v %v", ok, err)
	}
}
