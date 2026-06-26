package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// #57: dashboardTokenAuth gates everything except /healthz behind a Bearer.
func TestDashboardTokenAuth(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret"))
	})
	h := dashboardTokenAuth("s3cret-token", inner)

	check := func(path, authHdr string, wantCode int) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		if authHdr != "" {
			req.Header.Set("Authorization", authHdr)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != wantCode {
			t.Errorf("%s auth=%q -> %d, want %d", path, authHdr, rr.Code, wantCode)
		}
	}

	check("/dashboard/", "", http.StatusUnauthorized)             // no token
	check("/dashboard/", "Bearer wrong", http.StatusUnauthorized) // wrong token
	check("/dashboard/", "Bearer s3cret-token", http.StatusOK)    // right token
	check("/metrics", "Bearer s3cret-token", http.StatusOK)
	check("/metrics", "", http.StatusUnauthorized)
	check("/healthz", "", http.StatusOK) // healthz always open for probes
}
