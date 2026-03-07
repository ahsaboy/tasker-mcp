package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "adds leading slash", in: "mcp", want: "/mcp"},
		{name: "keeps existing slash", in: "/healthz", want: "/healthz"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePath(tc.in); got != tc.want {
				t.Fatalf("normalizePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsHeaderAuthEnabled(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  bool
	}{
		{name: "both set", key: "X-Tasker-Token", value: "secret", want: true},
		{name: "missing name", key: "", value: "secret", want: false},
		{name: "blank name", key: "   ", value: "secret", want: false},
		{name: "missing value", key: "X-Tasker-Token", value: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHeaderAuthEnabled(tc.key, tc.value); got != tc.want {
				t.Fatalf("isHeaderAuthEnabled(%q, %q) = %v, want %v", tc.key, tc.value, got, tc.want)
			}
		})
	}
}

func TestWithHeaderAuthDisabledPassesThrough(t *testing.T) {
	called := false
	handler := withHeaderAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}), "", "")

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Fatal("expected next handler to be called when auth is disabled")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestWithHeaderAuthRejectsMissingHeader(t *testing.T) {
	handler := withHeaderAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected call to next handler")
	}), "X-Tasker-Token", "secret")

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestWithHeaderAuthRejectsWrongValue(t *testing.T) {
	handler := withHeaderAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected call to next handler")
	}), "X-Tasker-Token", "secret")

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-Tasker-Token", "wrong")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestWithHeaderAuthPassesAuthorizedRequest(t *testing.T) {
	called := false
	handler := withHeaderAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}), "X-Tasker-Token", "secret")

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-Tasker-Token", "secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Fatal("expected next handler to be called for authorized request")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusAccepted)
	}
}
