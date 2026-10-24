package ingestor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDetectAPIVersion(t *testing.T) {
	serves := map[string]bool{"/slurmdb/v0.0.43/ping": true, "/slurmdb/v0.0.42/ping": true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serves[r.URL.Path] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, err := detectAPIVersion(context.Background(), srv.Client(), srv.URL, "slurm", "token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.0.43" {
		t.Fatalf("got %s, want the newest served version v0.0.43", got)
	}
}

func TestDetectAPIVersionRejectedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := detectAPIVersion(context.Background(), srv.Client(), srv.URL, "slurm", "bad")
	if err == nil || !strings.Contains(err.Error(), "rejected the token") {
		t.Fatalf("got %v, want a token error", err)
	}
}

func TestDetectAPIVersionNoneServed(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := detectAPIVersion(context.Background(), srv.Client(), srv.URL, "slurm", "token")
	if err == nil || !strings.Contains(err.Error(), "set SLURM_API_VERSION") {
		t.Fatalf("got %v, want a none-served error", err)
	}
}
