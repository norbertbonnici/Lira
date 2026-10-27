package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lira/internal/controller"
)

// Request bodies are capped: the small default everywhere, the larger import
// limit only on the import routes.
func TestLimitBody(t *testing.T) {
	var read int
	var readErr error
	h := limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		read, readErr = len(b), err
	}))
	post := func(path string, size int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("a", size)))
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	post("/login", defaultBodyLimit)
	if readErr != nil || read != defaultBodyLimit {
		t.Errorf("body at the default limit: read %d, err %v", read, readErr)
	}
	post("/login", defaultBodyLimit+1)
	if readErr == nil {
		t.Errorf("body over the default limit on /login was read in full (%d bytes)", read)
	}
	post("/transactions/import/commit", 2*defaultBodyLimit)
	if readErr != nil {
		t.Errorf("import route refused a body under ImportBodyLimit: %v", readErr)
	}
	post("/transactions/import/preview", controller.ImportBodyLimit+1)
	if readErr == nil {
		t.Error("import route accepted a body over ImportBodyLimit")
	}
}
