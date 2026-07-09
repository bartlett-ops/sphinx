package main

import "testing"

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
