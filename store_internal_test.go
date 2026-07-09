package main

import (
	"testing"
	"time"
)

func TestStoreUpsert(t *testing.T) {
	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	t.Run("new user increments generation", func(t *testing.T) {
		s := newStore()
		if !s.upsert("alice@example.com", "203.0.113.7/32", now) {
			t.Fatal("upsert of a new user should report a change")
		}
		if s.Generation != 1 {
			t.Errorf("Generation = %d, want 1", s.Generation)
		}
		if got := s.Users["alice@example.com"].CIDR; got != "203.0.113.7/32" {
			t.Errorf("CIDR = %q, want %q", got, "203.0.113.7/32")
		}
	})

	t.Run("unchanged cidr is a no-op", func(t *testing.T) {
		s := newStore()
		s.upsert("alice@example.com", "203.0.113.7/32", now)
		if s.upsert("alice@example.com", "203.0.113.7/32", later) {
			t.Fatal("re-upsert of an identical cidr should report no change")
		}
		if s.Generation != 1 {
			t.Errorf("Generation = %d, want 1 (unchanged)", s.Generation)
		}
		if got := s.Users["alice@example.com"].UpdatedAt; !got.Equal(now) {
			t.Errorf("UpdatedAt = %v, want %v (must not be refreshed)", got, now)
		}
	})

	// This is the regression test for the reported bug, at the domain level.
	t.Run("changed cidr replaces the old one", func(t *testing.T) {
		s := newStore()
		s.upsert("alice@example.com", "203.0.113.7/32", now)
		if !s.upsert("alice@example.com", "198.51.100.4/32", later) {
			t.Fatal("upsert of a changed cidr should report a change")
		}
		if len(s.Users) != 1 {
			t.Fatalf("len(Users) = %d, want 1 record per email", len(s.Users))
		}
		if s.Generation != 2 {
			t.Errorf("Generation = %d, want 2", s.Generation)
		}
		got := s.CIDRs()
		if len(got) != 1 || got[0] != "198.51.100.4/32" {
			t.Errorf("CIDRs() = %v, want exactly [198.51.100.4/32]", got)
		}
	})
}

func TestStoreCIDRs(t *testing.T) {
	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)

	t.Run("empty store", func(t *testing.T) {
		if got := newStore().CIDRs(); len(got) != 0 {
			t.Errorf("CIDRs() = %v, want empty", got)
		}
	})

	t.Run("sorted and deduplicated", func(t *testing.T) {
		s := newStore()
		s.upsert("carol@example.com", "203.0.113.9/32", now)
		s.upsert("alice@example.com", "198.51.100.4/32", now)
		// bob shares carol's address (shared NAT); it must appear once.
		s.upsert("bob@example.com", "203.0.113.9/32", now)

		got := s.CIDRs()
		want := []string{"198.51.100.4/32", "203.0.113.9/32"}
		if len(got) != len(want) {
			t.Fatalf("CIDRs() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("CIDRs()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
}
