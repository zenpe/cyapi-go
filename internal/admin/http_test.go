package admin

import (
	"context"
	"cyapi-go/internal/sourcecheck"
	"cyapi-go/internal/sourcerecommendation"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPermissionsSampleCASAndStatic(t *testing.T) {
	svc := &sourcecheck.Service{Directory: t.TempDir()}
	c := sourcecheck.NewController(context.Background(), svc)
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			next(w, r)
		}
	}
	h := New(svc, c, nil, guard, []int64{7}, func(_ context.Context, p, id string) (sourcecheck.Metadata, error) {
		return sourcecheck.Metadata{TrackID: id, Title: "Test", Artist: "Artist", DurationMS: 200000}, nil
	}, slog.Default())
	h.userID = func(r *http.Request) int64 {
		if r.Header.Get("Authorization") == "admin" {
			return 7
		}
		return 8
	}
	h.WithRecommendations(sourcerecommendation.New(t.TempDir(), t.TempDir(), nil, svc), false)
	mux := http.NewServeMux()
	h.Register(mux)
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for token, want := range map[string]int{"": 401, "user": 403, "admin": 200} {
		w := call("GET", "/admin/source-checks/samples", token, "")
		if w.Code != want {
			t.Fatal(token, w.Code)
		}
	}
	for token, want := range map[string]int{"": 401, "user": 403} {
		for _, method := range []string{"GET", "POST"} {
			if w := call(method, "/admin/source-recommendations", token, `{}`); w.Code != want {
				t.Fatal(method, token, w.Code)
			}
		}
	}
	if w := call("GET", "/admin/source-recommendations", "admin", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatal("recommendation admin", w.Code, w.Body.String())
	}
	w := call("GET", "/admin/source-checks/samples", "admin", "")
	var state struct{ Version string }
	json.Unmarshal(w.Body.Bytes(), &state)
	body := `{"platform":"qq","trackId":"test123","expectedVersion":"` + state.Version + `"}`
	w = call("POST", "/admin/source-checks/samples", "admin", body)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call("POST", "/admin/source-checks/samples", "admin", body)
	if w.Code != 409 {
		t.Fatal("stale write", w.Code)
	}
	w = call("POST", "/admin/source-checks/samples", "admin", `{"unknown":true}`)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = call("GET", "/admin/source-management", "", "")
	if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("page", w.Code)
	}
	w = call("GET", "/admin/source-management/assets/app.js", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "source-checks") {
		t.Fatal("asset", w.Code)
	}
}
