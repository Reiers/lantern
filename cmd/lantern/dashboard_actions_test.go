// Tests for issue #14 action endpoints: same-origin guard, POST-only,
// and the deps-nil paths return clean error responses.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestActionPreflight covers the three reject paths + the accept path.
func TestActionPreflight(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		origin     string
		wantStatus int
		wantPass   bool
	}{
		{"reject_get", "GET", "dashboard", http.StatusMethodNotAllowed, false},
		{"reject_put", "PUT", "dashboard", http.StatusMethodNotAllowed, false},
		{"reject_missing_origin", "POST", "", http.StatusForbidden, false},
		{"reject_wrong_origin", "POST", "evil-site", http.StatusForbidden, false},
		{"accept_post_with_origin", "POST", "dashboard", http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/dashboard/actions/find-peers", nil)
			if tc.origin != "" {
				req.Header.Set("X-Lantern-Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			pass := actionPreflight(w, req)
			if pass != tc.wantPass {
				t.Errorf("pass=%v, want %v", pass, tc.wantPass)
			}
			if !pass && w.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}

// TestActionFindPeers_NoHost: the handler must degrade cleanly when no
// libp2p host is configured (e.g., a daemon run with --no-libp2p).
func TestActionFindPeers_NoHost(t *testing.T) {
	deps := &dashboardDeps{}
	res := deps.actionFindPeers(context.Background())
	if res.Status != "error" {
		t.Errorf("status = %q, want error", res.Status)
	}
	if !strings.Contains(res.Message, "host") {
		t.Errorf("message %q doesn't mention 'host'", res.Message)
	}
}

// TestActionRenewAnchor_NoDataDir: similarly, no data dir means we can't
// write the anchor file. Refuse cleanly.
func TestActionRenewAnchor_NoDataDir(t *testing.T) {
	deps := &dashboardDeps{} // dataDirPath = ""
	res := deps.actionRenewAnchor(context.Background())
	if res.Status != "error" {
		t.Errorf("status = %q, want error", res.Status)
	}
	if !strings.Contains(res.Message, "data directory") {
		t.Errorf("message %q doesn't mention 'data directory'", res.Message)
	}
}

// TestActionResultJSON: the wire shape is stable so the dashboard JS
// keeps working after refactors.
func TestActionResultJSON(t *testing.T) {
	res := actionResult{
		Status:  "ok",
		Message: "all good",
		Detail:  map[string]any{"peers_before": 10, "peers_after": 25},
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	for _, want := range []string{`"status":"ok"`, `"message":"all good"`, `"peers_before":10`, `"peers_after":25`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}

	// Detail must omit when empty.
	noDetail := actionResult{Status: "error", Message: "boom"}
	raw2, _ := json.Marshal(noDetail)
	if strings.Contains(string(raw2), "detail") {
		t.Errorf("expected omitempty on Detail; got %s", raw2)
	}
}
