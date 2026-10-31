package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoutes_AnswerTheHealthCheckAndLeaveTheHookAndLogsToTheHandler(t *testing.T) {
	srv := httptest.NewServer(routes(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/hook": http.StatusAccepted, "/logs": http.StatusAccepted} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("GET %s answered %d, want %d", path, res.StatusCode, want)
		}
		if path == "/healthz" && string(body) != "ok" {
			t.Errorf("GET /healthz answered %q, want ok", body)
		}
	}
}
