package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
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

// --- fake dynamic client helpers -------------------------------------------

func testScheme(t *testing.T) (*runtime.Scheme, map[schema.GroupVersionResource]string) {
	t.Helper()
	return runtime.NewScheme(), map[schema.GroupVersionResource]string{
		configMapGVR:  "ConfigMapList",
		middlewareGVR: "MiddlewareList",
	}
}

func newFakeClient(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme, listKinds := testScheme(t)
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)
}

func configMapWith(t *testing.T, data map[string]string) *unstructured.Unstructured {
	t.Helper()
	d := make(map[string]any, len(data))
	for k, v := range data {
		d[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "sphinx-users", "namespace": "kube-system"},
		"data":       d,
	}}
}

func storeJSON(t *testing.T, s *Store) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	return string(b)
}

// readStore fetches users.json straight from the fake and decodes it.
func readStore(t *testing.T, c dynamic.Interface) *Store {
	t.Helper()
	u, err := c.Resource(configMapGVR).Namespace("kube-system").
		Get(context.Background(), "sphinx-users", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	s, err := decodeStore(u)
	if err != nil {
		t.Fatalf("decode store: %v", err)
	}
	return s
}

// --- tests ------------------------------------------------------------------

func TestConfigMapStoreLoad(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)

	t.Run("absent configmap yields an empty store", func(t *testing.T) {
		c := newFakeClient(t)
		s, err := newConfigMapStore(c, "kube-system", "sphinx-users").Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(s.Users) != 0 || s.Generation != 0 {
			t.Errorf("Load() = %+v, want empty store", s)
		}
	})

	t.Run("decodes an existing document", func(t *testing.T) {
		seed := newStore()
		seed.upsert("alice@example.com", "203.0.113.7/32", now)
		c := newFakeClient(t, configMapWith(t, map[string]string{storeKey: storeJSON(t, seed)}))

		s, err := newConfigMapStore(c, "kube-system", "sphinx-users").Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := s.Users["alice@example.com"].CIDR; got != "203.0.113.7/32" {
			t.Errorf("CIDR = %q, want %q", got, "203.0.113.7/32")
		}
		if s.Generation != 1 {
			t.Errorf("Generation = %d, want 1", s.Generation)
		}
	})
}

func TestConfigMapStoreCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("absent configmap is not a failure", func(t *testing.T) {
		c := newFakeClient(t)
		if err := newConfigMapStore(c, "kube-system", "sphinx-users").Check(ctx); err != nil {
			t.Errorf("Check on absent configmap = %v, want nil; a fresh install has no users", err)
		}
	})

	t.Run("present configmap is reachable", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{storeKey: storeJSON(t, newStore())}))
		if err := newConfigMapStore(c, "kube-system", "sphinx-users").Check(ctx); err != nil {
			t.Errorf("Check = %v, want nil", err)
		}
	})
}

func TestConfigMapStoreUpsert(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)

	t.Run("creates the configmap when absent", func(t *testing.T) {
		c := newFakeClient(t)
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		s, err := st.Upsert(ctx, "alice@example.com", "203.0.113.7/32", now)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if s.Generation != 1 {
			t.Errorf("Generation = %d, want 1", s.Generation)
		}
		if got := readStore(t, c).Users["alice@example.com"].CIDR; got != "203.0.113.7/32" {
			t.Errorf("persisted CIDR = %q, want %q", got, "203.0.113.7/32")
		}
	})

	t.Run("replaces a changed cidr in place", func(t *testing.T) {
		seed := newStore()
		seed.upsert("alice@example.com", "203.0.113.7/32", now)
		c := newFakeClient(t, configMapWith(t, map[string]string{storeKey: storeJSON(t, seed)}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		if _, err := st.Upsert(ctx, "alice@example.com", "198.51.100.4/32", now); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		got := readStore(t, c)
		if len(got.Users) != 1 {
			t.Fatalf("len(Users) = %d, want 1", len(got.Users))
		}
		if got.Users["alice@example.com"].CIDR != "198.51.100.4/32" {
			t.Errorf("CIDR = %q, want the new one", got.Users["alice@example.com"].CIDR)
		}
	})

	t.Run("unchanged cidr issues no write", func(t *testing.T) {
		seed := newStore()
		seed.upsert("alice@example.com", "203.0.113.7/32", now)
		c := newFakeClient(t, configMapWith(t, map[string]string{storeKey: storeJSON(t, seed)}))

		var updates int
		c.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			updates++
			return false, nil, nil
		})

		st := newConfigMapStore(c, "kube-system", "sphinx-users")
		if _, err := st.Upsert(ctx, "alice@example.com", "203.0.113.7/32", now); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if updates != 0 {
			t.Errorf("update calls = %d, want 0 for an unchanged cidr", updates)
		}
	})

	// The fake does not enforce resourceVersion, so the conflict is injected.
	// The reactor also writes a competing user, proving the retry re-reads
	// rather than clobbering.
	t.Run("retries on conflict and re-reads", func(t *testing.T) {
		seed := newStore()
		c := newFakeClient(t, configMapWith(t, map[string]string{storeKey: storeJSON(t, seed)}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		var fired bool
		c.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			if fired {
				return false, nil, nil
			}
			fired = true
			// A competing replica lands first. This must go through the
			// object tracker directly rather than c.Resource(...).Update:
			// Fake.Invokes holds a non-reentrant lock for the whole reactor
			// call, so a nested call back through the dynamic client here
			// would deadlock against the outer Update that is dispatching
			// this reactor.
			competitor := newStore()
			competitor.upsert("bob@example.com", "198.51.100.4/32", now)
			cm := configMapWith(t, map[string]string{storeKey: storeJSON(t, competitor)})
			if err := c.Tracker().Update(configMapGVR, cm, "kube-system"); err != nil {
				t.Errorf("competitor update: %v", err)
			}
			return true, nil, k8serrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, "sphinx-users", errors.New("stale"))
		})

		if _, err := st.Upsert(ctx, "alice@example.com", "203.0.113.7/32", now); err != nil {
			t.Fatalf("Upsert should recover from a conflict: %v", err)
		}
		got := readStore(t, c)
		if _, ok := got.Users["bob@example.com"]; !ok {
			t.Error("competing writer's record was clobbered; the retry did not re-read")
		}
		if _, ok := got.Users["alice@example.com"]; !ok {
			t.Error("our record was not written")
		}
	})

	t.Run("gives up after maxRetries conflicts", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{storeKey: storeJSON(t, newStore())}))
		c.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, k8serrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, "sphinx-users", errors.New("stale"))
		})
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		if _, err := st.Upsert(ctx, "alice@example.com", "203.0.113.7/32", now); err == nil {
			t.Fatal("Upsert should fail after exhausting retries")
		}
	})

	t.Run("abandons the retry loop when ctx is cancelled", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{storeKey: storeJSON(t, newStore())}))
		cancelCtx, cancel := context.WithCancel(context.Background())
		c.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			cancel() // the caller gives up while we are mid-retry
			return true, nil, k8serrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, "sphinx-users", errors.New("stale"))
		})
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		if _, err := st.Upsert(cancelCtx, "alice@example.com", "203.0.113.7/32", now); !errors.Is(err, context.Canceled) {
			t.Fatalf("Upsert error = %v, want context.Canceled; a cancelled caller must not wait out the backoff", err)
		}
	})
}

func TestConfigMapStoreMigrate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)

	legacyBlob := func(pairs map[string]string) string {
		t.Helper()
		m := make(map[string]legacyUser, len(pairs))
		for email, ip := range pairs {
			m[email] = legacyUser{Email: email, IP: ip}
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal legacy blob: %v", err)
		}
		return string(b)
	}

	t.Run("merges disjoint pod blobs and drops legacy keys", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{
			"sphinx-abc123": legacyBlob(map[string]string{"alice@example.com": "203.0.113.7"}),
			"sphinx-def456": legacyBlob(map[string]string{"bob@example.com": "198.51.100.4"}),
		}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		if err := st.Migrate(ctx, now); err != nil {
			t.Fatalf("Migrate: %v", err)
		}

		got := readStore(t, c)
		if len(got.Users) != 2 {
			t.Fatalf("len(Users) = %d, want 2", len(got.Users))
		}
		if got.Users["alice@example.com"].CIDR != "203.0.113.7/32" {
			t.Errorf("alice CIDR = %q, want 203.0.113.7/32", got.Users["alice@example.com"].CIDR)
		}
		if !got.Users["alice@example.com"].UpdatedAt.Equal(now) {
			t.Errorf("alice UpdatedAt = %v, want migration time %v", got.Users["alice@example.com"].UpdatedAt, now)
		}

		u, err := c.Resource(configMapGVR).Namespace("kube-system").
			Get(ctx, "sphinx-users", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get configmap: %v", err)
		}
		data, _, err := unstructured.NestedStringMap(u.Object, "data")
		if err != nil {
			t.Fatalf("read data: %v", err)
		}
		if len(data) != 1 {
			t.Errorf("data keys = %v, want only %s", data, storeKey)
		}
	})

	t.Run("drops a user whose blobs disagree", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{
			"sphinx-abc123": legacyBlob(map[string]string{"alice@example.com": "203.0.113.7"}),
			"sphinx-def456": legacyBlob(map[string]string{"alice@example.com": "198.51.100.4"}),
			"sphinx-ghi789": legacyBlob(map[string]string{"bob@example.com": "192.0.2.9"}),
		}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		if err := st.Migrate(ctx, now); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		got := readStore(t, c)
		if _, ok := got.Users["alice@example.com"]; ok {
			t.Error("alice has conflicting CIDRs and must be dropped, forcing re-authentication")
		}
		if _, ok := got.Users["bob@example.com"]; !ok {
			t.Error("bob is unambiguous and must survive")
		}
	})

	t.Run("agreeing blobs keep the user", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{
			"sphinx-abc123": legacyBlob(map[string]string{"alice@example.com": "203.0.113.7"}),
			"sphinx-def456": legacyBlob(map[string]string{"alice@example.com": "203.0.113.7"}),
		}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		if err := st.Migrate(ctx, now); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		if got := readStore(t, c); got.Users["alice@example.com"].CIDR != "203.0.113.7/32" {
			t.Errorf("alice CIDR = %q, want 203.0.113.7/32", got.Users["alice@example.com"].CIDR)
		}
	})

	t.Run("skips a user with an unparseable ip", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{
			"sphinx-abc123": legacyBlob(map[string]string{"alice@example.com": "not-an-ip"}),
			"sphinx-def456": legacyBlob(map[string]string{"bob@example.com": "198.51.100.4"}),
		}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		if err := st.Migrate(ctx, now); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		got := readStore(t, c)
		if _, ok := got.Users["alice@example.com"]; ok {
			t.Error("alice has an unparseable ip and must be skipped")
		}
		if len(got.Users) != 1 {
			t.Errorf("len(Users) = %d, want 1", len(got.Users))
		}
	})

	t.Run("is a no-op when users.json already exists", func(t *testing.T) {
		seed := newStore()
		seed.upsert("alice@example.com", "203.0.113.7/32", now)
		c := newFakeClient(t, configMapWith(t, map[string]string{
			storeKey:        storeJSON(t, seed),
			"sphinx-abc123": legacyBlob(map[string]string{"bob@example.com": "198.51.100.4"}),
		}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		if err := st.Migrate(ctx, now); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		got := readStore(t, c)
		if _, ok := got.Users["bob@example.com"]; ok {
			t.Error("migration must not run once users.json exists")
		}
	})

	t.Run("is a no-op when the configmap is absent", func(t *testing.T) {
		c := newFakeClient(t)
		st := newConfigMapStore(c, "kube-system", "sphinx-users")
		if err := st.Migrate(ctx, now); err != nil {
			t.Fatalf("Migrate on absent configmap should succeed: %v", err)
		}
	})

	// Every replica but one loses the migration race on the cutover rollout.
	// Losing means the work is already done, not that startup failed.
	t.Run("a lost migration race is not an error", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{
			"sphinx-abc123": legacyBlob(map[string]string{"alice@example.com": "203.0.113.7"}),
		}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		c.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			// The winning replica writes users.json first. Staged through the
			// tracker: a reactor must not call back into the client.
			winner := newStore()
			winner.upsert("alice@example.com", "203.0.113.7/32", now)
			cm := configMapWith(t, map[string]string{storeKey: storeJSON(t, winner)})
			if err := c.Tracker().Update(configMapGVR, cm, "kube-system"); err != nil {
				t.Errorf("winner update: %v", err)
			}
			return true, nil, k8serrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, "sphinx-users", errors.New("stale"))
		})

		if err := st.Migrate(ctx, now); err != nil {
			t.Fatalf("losing the migration race must not be an error: %v", err)
		}
	})

	t.Run("a conflict that did not migrate still surfaces", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{
			"sphinx-abc123": legacyBlob(map[string]string{"alice@example.com": "203.0.113.7"}),
		}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		c.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, k8serrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, "sphinx-users", errors.New("stale"))
		})

		if err := st.Migrate(ctx, now); err == nil {
			t.Fatal("a conflict with no migrated document must surface as an error")
		}
	})

	// An old pod touches the ConfigMap between our Get and Update. That is a
	// conflict, but nobody migrated: we must re-read and retry, not die.
	t.Run("retries when a legacy pod bumps the configmap", func(t *testing.T) {
		c := newFakeClient(t, configMapWith(t, map[string]string{
			"sphinx-abc123": legacyBlob(map[string]string{"alice@example.com": "203.0.113.7"}),
		}))
		st := newConfigMapStore(c, "kube-system", "sphinx-users")

		var fired bool
		c.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			if fired {
				return false, nil, nil
			}
			fired = true
			// A legacy pod re-adds its own key. Staged through the tracker: a
			// reactor must not call back into the client.
			cm := configMapWith(t, map[string]string{
				"sphinx-abc123": legacyBlob(map[string]string{"alice@example.com": "203.0.113.7"}),
				"sphinx-def456": legacyBlob(map[string]string{"bob@example.com": "198.51.100.4"}),
			})
			if err := c.Tracker().Update(configMapGVR, cm, "kube-system"); err != nil {
				t.Errorf("legacy pod update: %v", err)
			}
			return true, nil, k8serrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, "sphinx-users", errors.New("stale"))
		})

		if err := st.Migrate(ctx, now); err != nil {
			t.Fatalf("Migrate must retry a conflict when nobody migrated: %v", err)
		}
		got := readStore(t, c)
		// The retry re-read, so it picked up the legacy pod's newly added user.
		if _, ok := got.Users["bob@example.com"]; !ok {
			t.Error("retry did not re-read: bob's legacy record was lost")
		}
		if _, ok := got.Users["alice@example.com"]; !ok {
			t.Error("alice's legacy record was lost")
		}
	})
}
