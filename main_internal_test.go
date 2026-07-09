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

// Gin's ClientIP walks X-Forwarded-For right-to-left, skipping trusted proxies,
// so an attacker-injected leftmost entry is ignored.
func TestClientIPHonoursTrustedProxies(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		giveTrusted    []string
		giveRemoteAddr string
		giveXFF        string
		want           string
	}{
		{
			name:           "single hop from a trusted proxy",
			giveTrusted:    []string{"10.42.0.0/16"},
			giveRemoteAddr: "10.42.0.154:34567",
			giveXFF:        "92.40.212.206",
			want:           "92.40.212.206/32",
		},
		{
			// The spoofed leftmost entry must be ignored.
			name:           "injected leftmost entry is ignored",
			giveTrusted:    []string{"10.42.0.0/16"},
			giveRemoteAddr: "10.42.0.154:34567",
			giveXFF:        "8.8.8.8, 92.40.212.206",
			want:           "92.40.212.206/32",
		},
		{
			name:           "ipv6 client",
			giveTrusted:    []string{"10.42.0.0/16"},
			giveRemoteAddr: "10.42.0.154:34567",
			giveXFF:        "2001:db8::1",
			want:           "2001:db8::1/128",
		},
		{
			name:           "untrusted direct connection ignores the header entirely",
			giveTrusted:    []string{"10.42.0.0/16"},
			giveRemoteAddr: "203.0.113.9:5555",
			giveXFF:        "8.8.8.8",
			want:           "203.0.113.9/32",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			router := gin.New()
			if err := router.SetTrustedProxies(tt.giveTrusted); err != nil {
				t.Fatalf("SetTrustedProxies: %v", err)
			}
			router.GET("/x", func(c *gin.Context) {
				cidr, err := hostCIDR(c.ClientIP())
				if err != nil {
					t.Errorf("hostCIDR: %v", err)
					return
				}
				got = cidr
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = tt.giveRemoteAddr
			if tt.giveXFF != "" {
				req.Header.Set("X-Forwarded-For", tt.giveXFF)
			}
			router.ServeHTTP(httptest.NewRecorder(), req)

			if got != tt.want {
				t.Errorf("client CIDR = %q, want %q", got, tt.want)
			}
		})
	}
}

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

func TestCatchAllProxies(t *testing.T) {
	tests := []struct {
		name string
		give []string
		want bool
	}{
		{name: "empty", give: nil, want: false},
		{name: "narrow ipv4", give: []string{"10.42.0.0/16"}, want: false},
		{name: "loopback", give: []string{"127.0.0.1/32"}, want: false},
		{name: "ipv4 catch-all", give: []string{"0.0.0.0/0"}, want: true},
		{name: "ipv6 catch-all", give: []string{"::/0"}, want: true},
		{name: "catch-all hidden among narrow entries", give: []string{"10.42.0.0/16", "0.0.0.0/0"}, want: true},
		{name: "whitespace tolerated", give: []string{" 0.0.0.0/0 "}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := catchAllProxies(tt.give); got != tt.want {
				t.Errorf("catchAllProxies(%v) = %v, want %v", tt.give, got, tt.want)
			}
		})
	}
}
