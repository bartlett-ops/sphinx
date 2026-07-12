package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeStore struct {
	store     *Store
	loads     int
	upserts   int
	loadErr   error
	upsertErr error
}

func (f *fakeStore) Load(context.Context) (*Store, error) {
	f.loads++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.store, nil
}

func (f *fakeStore) Upsert(_ context.Context, email, cidr string, now time.Time) (*Store, error) {
	f.upserts++
	if f.upsertErr != nil {
		return nil, f.upsertErr
	}
	f.store.upsert(email, cidr, now)
	return f.store, nil
}

type fakeAllowlist struct {
	calls   int
	last    []string
	lastGen int64
	lastUID string
	err     error
}

func (f *fakeAllowlist) Apply(_ context.Context, cidrs []string, generation int64, storeUID string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.last = append([]string(nil), cidrs...)
	f.lastGen = generation
	f.lastUID = storeUID
	return nil
}

func newFakes() (*fakeStore, *fakeAllowlist, *Reconciler) {
	fs := &fakeStore{store: newStore()}
	fa := &fakeAllowlist{}
	return fs, fa, newReconciler(fs, fa)
}

// The reported bug, end to end through the reconciler.
func TestReconcilerReAuthReplacesCIDR(t *testing.T) {
	ctx := context.Background()
	fs, fa, r := newFakes()

	if _, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32"); err != nil {
		t.Fatalf("first Authenticate: %v", err)
	}
	if _, err := r.Authenticate(ctx, "alice@example.com", "198.51.100.4/32"); err != nil {
		t.Fatalf("second Authenticate: %v", err)
	}

	if len(fs.store.Users) != 1 {
		t.Fatalf("len(Users) = %d, want 1 record for one email", len(fs.store.Users))
	}
	if !equalStrings(fa.last, []string{"198.51.100.4/32"}) {
		t.Errorf("applied CIDRs = %v, want exactly [198.51.100.4/32]; the old CIDR must be gone", fa.last)
	}
}

func TestReconcilerWriteSkipCache(t *testing.T) {
	ctx := context.Background()

	t.Run("repeated identical auth issues no writes", func(t *testing.T) {
		fs, fa, r := newFakes()

		wrote, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32")
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if !wrote {
			t.Error("first Authenticate should report a write")
		}

		for range 5 {
			wrote, err = r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32")
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if wrote {
				t.Error("repeated Authenticate should be served from cache")
			}
		}
		if fs.upserts != 1 {
			t.Errorf("upserts = %d, want 1; ForwardAuth fires on every request", fs.upserts)
		}
		if fa.calls != 1 {
			t.Errorf("allowlist applies = %d, want 1", fa.calls)
		}
	})

	t.Run("changed cidr bypasses the cache", func(t *testing.T) {
		fs, _, r := newFakes()
		if _, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32"); err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		wrote, err := r.Authenticate(ctx, "alice@example.com", "198.51.100.4/32")
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if !wrote {
			t.Error("a changed cidr must not be served from cache")
		}
		if fs.upserts != 2 {
			t.Errorf("upserts = %d, want 2", fs.upserts)
		}
	})

	t.Run("apply failure leaves the cache cold", func(t *testing.T) {
		fs, fa, r := newFakes()
		fa.err = errors.New("api server down")

		if _, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32"); err == nil {
			t.Fatal("Authenticate should surface the apply failure")
		}
		fa.err = nil
		wrote, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32")
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if !wrote {
			t.Error("a failed apply must not be cached as success")
		}
		if fs.upserts != 2 {
			t.Errorf("upserts = %d, want 2", fs.upserts)
		}
	})
}

func TestReconcileRefreshesCache(t *testing.T) {
	ctx := context.Background()
	fs, _, r := newFakes()

	if _, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	// Another replica moves alice.
	fs.store.upsert("alice@example.com", "198.51.100.4/32", time.Now())

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The stale cache entry must not mask the change.
	wrote, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !wrote {
		t.Error("Reconcile must refresh the cache so a stale entry cannot short-circuit")
	}
}

func TestReconcilerPodReplacement(t *testing.T) {
	ctx := context.Background()
	fs, fa, r := newFakes()

	if _, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	before := append([]string(nil), fa.last...)

	// The pod is replaced: a brand new Reconciler over the same store.
	fresh := newReconciler(fs, fa)
	if err := fresh.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !equalStrings(fa.last, before) {
		t.Errorf("CIDRs after replacement = %v, want %v; no orphans, no growth", fa.last, before)
	}
	if len(fs.store.Users) != 1 {
		t.Errorf("len(Users) = %d, want 1", len(fs.store.Users))
	}
}

func TestReconcilerPersistsAcrossTicks(t *testing.T) {
	ctx := context.Background()
	fs, fa, r := newFakes()

	if _, err := r.Authenticate(ctx, "alice@example.com", "203.0.113.7/32"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	for range 10 {
		if err := r.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if len(fs.store.Users) != 1 {
		t.Errorf("len(Users) = %d, want 1; records never expire", len(fs.store.Users))
	}
	if !equalStrings(fa.last, []string{"203.0.113.7/32"}) {
		t.Errorf("applied CIDRs = %v, want [203.0.113.7/32]", fa.last)
	}
}

func TestReconcilerHealthy(t *testing.T) {
	ctx := context.Background()
	fs, _, r := newFakes()
	now := time.Now()

	if r.Healthy(now, time.Minute) {
		t.Error("a reconciler that has never reconciled must not be healthy")
	}
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !r.Healthy(time.Now(), time.Minute) {
		t.Error("a freshly reconciled reconciler must be healthy")
	}
	if r.Healthy(time.Now().Add(2*time.Hour), time.Minute) {
		t.Error("a stale reconcile must not be healthy")
	}

	fs.loadErr = errors.New("api server down")
	if err := r.Reconcile(ctx); err == nil {
		t.Error("Reconcile should surface a load failure")
	}
}
