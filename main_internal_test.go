package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type stubChecker struct{ err error }

func (s stubChecker) Check(context.Context) error { return s.err }

func TestReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// A reconciler that has succeeded just now.
	healthy := func(t *testing.T) *Reconciler {
		t.Helper()
		r := newReconciler(&fakeStore{store: newStore()}, &fakeAllowlist{})
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		return r
	}

	tests := []struct {
		name           string
		giveReconciler func(*testing.T) *Reconciler
		giveAllowlist  checker
		giveStore      checker
		wantStatus     int
	}{
		{
			name:           "ready",
			giveReconciler: healthy,
			giveAllowlist:  stubChecker{},
			giveStore:      stubChecker{},
			wantStatus:     http.StatusOK,
		},
		{
			name:           "never reconciled",
			giveReconciler: func(*testing.T) *Reconciler { return newReconciler(&fakeStore{store: newStore()}, &fakeAllowlist{}) },
			giveAllowlist:  stubChecker{},
			giveStore:      stubChecker{},
			wantStatus:     http.StatusServiceUnavailable,
		},
		{
			// The failure mode a freshness check alone cannot see.
			name:           "middleware unreachable despite a recent reconcile",
			giveReconciler: healthy,
			giveAllowlist:  stubChecker{err: errors.New("connection refused")},
			giveStore:      stubChecker{},
			wantStatus:     http.StatusServiceUnavailable,
		},
		{
			name:           "configmap unreachable despite a recent reconcile",
			giveReconciler: healthy,
			giveAllowlist:  stubChecker{},
			giveStore:      stubChecker{err: errors.New("connection refused")},
			wantStatus:     http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/ready", readiness(tt.giveReconciler(t), tt.giveAllowlist, tt.giveStore, time.Minute))

			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))

			if w.Code != tt.wantStatus {
				t.Errorf("GET /ready = %d, want %d (body: %s)", w.Code, tt.wantStatus, w.Body.String())
			}
		})
	}
}

func TestHostCIDR(t *testing.T) {
	tests := []struct {
		name    string
		giveIP  string
		want    string
		wantErr bool
	}{
		{
			name:   "ipv4",
			giveIP: "203.0.113.7",
			want:   "203.0.113.7/32",
		},
		{
			name:   "ipv6",
			giveIP: "2001:db8::1",
			want:   "2001:db8::1/128",
		},
		{
			// A v4 address arriving over a v6 socket must not be widened to /128.
			name:   "ipv4-mapped ipv6 unmaps to /32",
			giveIP: "::ffff:192.0.2.1",
			want:   "192.0.2.1/32",
		},
		{
			name:   "ipv6 link-local drops zone",
			giveIP: "fe80::1%eth0",
			want:   "fe80::1/128",
		},
		{
			name:    "empty",
			giveIP:  "",
			wantErr: true,
		},
		{
			name:    "not an ip",
			giveIP:  "nonsense",
			wantErr: true,
		},
		{
			name:    "ip with port",
			giveIP:  "203.0.113.7:8080",
			wantErr: true,
		},
		{
			name:    "already a cidr",
			giveIP:  "203.0.113.7/32",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hostCIDR(tt.giveIP)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("hostCIDR(%q) = %q, want error", tt.giveIP, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("hostCIDR(%q) returned unexpected error: %v", tt.giveIP, err)
			}
			if got != tt.want {
				t.Errorf("hostCIDR(%q) = %q, want %q", tt.giveIP, got, tt.want)
			}
		})
	}
}
