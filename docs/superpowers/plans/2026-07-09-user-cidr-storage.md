# User/CIDR Storage Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Store one CIDR per user keyed by email, and make the Traefik allowlist an authoritative projection of that store, so a re-authenticating user's old CIDR is removed and replacing a pod strands no data.

**Architecture:** Three new files split along the seam that matters. `store.go` owns a single JSON document in a ConfigMap keyed by email, mutated by compare-and-swap on `resourceVersion`; it knows nothing of Traefik. `allowlist.go` owns the Traefik `Middleware`, replacing `spec.ipAllowList.sourceRange` wholesale and guarding against stale writes with a generation annotation; it knows nothing of users. `reconciler.go` glues them: both the auth path and a background ticker project the store onto the allowlist. `middleware.go` is deleted entirely.

**Tech Stack:** Go 1.25.8, `k8s.io/client-go` dynamic client, `net/netip`, Gin, `peterbourgon/ff/v3`. Tests use the standard library `testing` package and `k8s.io/client-go/dynamic/fake`.

## Global Constraints

- Go version: `1.25.8` (from `go.mod`). `for range N` integer-range loops are available and already used in this codebase.
- Single package `main`. All files live at repo root. Do **not** create subdirectories.
- Source file names follow the existing codebase convention: `store.go`, `allowlist.go`, `reconciler.go` (not kebab-case).
- Test file names follow the existing `main_internal_test.go` convention: `<name>_internal_test.go`, `package main`.
- **No testify.** It is an indirect dependency only. Tests use stdlib `testing` with explicit comparisons, matching `main_internal_test.go`.
- Every task must end with `go test ./...`, `go vet ./...`, and `gofmt -l .` all clean, and a green `go build ./...`.
- **`golangci-lint` currently cannot run**: the repo's `.golangci.yml` is v1 format and the installed binary is v2 (`can't load config: unsupported version of the configuration: ""`). This predates this work. Do **not** fix it as part of this plan and do **not** gate any task on it.
- Commit message style follows the existing log: imperative mood, no `feat:`/`fix:` prefix. Every commit ends with the trailer `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Package-level constants shared across files (`maxRetries`, `storeKey`, `storeVersion`, `generationAnnotation`) are declared exactly once, in the task that introduces them. Do not redeclare.
- The GVR variables `configMapGVR` and `middlewareGVR` already exist in `main.go`'s `var` block and are reused by the new files. They are **not** redeclared and **not** removed until Task 6.
- The `hostCIDR` function and its test already exist (commit `a256f2d`) and are reused, not rewritten.

## Spike Findings (already verified — do not re-derive)

These were confirmed empirically against `k8s.io/client-go v0.35.3` before this plan was written:

1. **The fake dynamic client does not track `resourceVersion`.** It stays `""` before and after `Update`. Therefore CAS behaviour **cannot** be tested by relying on the fake to reject a stale write. Conflicts must be injected with `PrependReactor` returning `k8serrors.NewConflict(...)`.
2. `Middleware` is a CRD absent from any scheme, so the fake must be built with `dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)` supplying both `ConfigMapList` and `MiddlewareList`.
3. A one-shot reactor (fire once, then return `false` to fall through to the default object tracker) works as expected.
4. `go mod tidy` is required once — the fake pulls in indirect test dependencies only. No new *direct* dependencies.
5. **A reactor must not call back into the client.** `k8stesting.Fake.Invokes` holds a non-reentrant lock across the reactor callback, so a nested `c.Resource(...).Update(...)` from inside a reactor deadlocks against the `Update` that dispatched it — 100% of the time, confirmed by goroutine dump. To stage a competing write from inside a reactor, use `c.Tracker().Update(gvr, obj, namespace)`, which bypasses that lock. (Discovered during Task 2; the plan's original code had this bug.)

## File Structure

| File | Responsibility |
|------|----------------|
| `store.go` (create) | `Record`, `Store`, pure `Store.upsert`/`Store.CIDRs`; `UserStore` interface; `configMapStore` (Load/Upsert/Migrate) with CAS retry. Knows nothing of Traefik. |
| `store_internal_test.go` (create) | Pure store logic + `configMapStore` against the fake dynamic client + migration. |
| `allowlist.go` (create) | `Allowlist` interface; `traefikAllowlist` (EnsureExists/Apply). Replaces `sourceRange` wholesale, guards on generation annotation. Knows nothing of users. |
| `allowlist_internal_test.go` (create) | Replace-not-union; generation guard; EnsureExists. |
| `reconciler.go` (create) | `Reconciler`: `project`, `Authenticate` (hot path + write-skip cache), `Reconcile` (drift repair + cache refresh), `Healthy`, `Run`. |
| `reconciler_internal_test.go` (create) | Regression test for the reported bug, write-skip cache, pod replacement, health. Uses in-memory fakes. |
| `main.go` (modify) | Flags, wiring, handlers. Loses `users`, `usersMu`, `instanceID`, `dynClient`, `user`, `getCIDRsFromUsers`, `getUnstructured`. Keeps `hostCIDR`, `resolveClientIP`, `resolveKubeConfig`, GVRs. |
| `middleware.go` (delete) | Entirely superseded by `allowlist.go`. |
| `README.md` (modify) | RBAC verbs, remove the "each instance owns its own key" paragraph, document `--reconcile-interval`. |

Tasks 1–5 only **add** files. Go permits unused package-level functions and types, so the tree compiles and every test passes at each commit even before `main.go` is rewired in Task 6.

---

### Task 1: Store domain types and pure projection

Pure, in-memory logic with no Kubernetes involvement. This is where "one CIDR per user" and "generation increments only on change" become true; everything later depends on it.

**Files:**
- Create: `store.go`
- Test: `store_internal_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const storeVersion = 1`, `const storeKey = "users.json"`, `const maxRetries = 5`
  - `type Record struct { CIDR string; UpdatedAt time.Time }`
  - `type Store struct { Version int; Generation int64; Users map[string]Record }`
  - `func newStore() *Store`
  - `func (s *Store) upsert(email, cidr string, now time.Time) bool` — reports whether the store changed
  - `func (s *Store) CIDRs() []string` — sorted, deduplicated

- [ ] **Step 1: Write the failing test**

Create `store_internal_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestStore' ./...`
Expected: FAIL, build error `undefined: newStore`

- [ ] **Step 3: Write minimal implementation**

Create `store.go`:

```go
package main

import (
	"sort"
	"time"
)

const (
	// storeVersion is the schema tag written into every persisted document.
	storeVersion = 1
	// storeKey is the single ConfigMap data key holding the whole store.
	storeKey = "users.json"
	// maxRetries bounds every compare-and-swap loop in this package.
	maxRetries = 5
)

// Record is one user's allowlisted CIDR. UpdatedAt records when the CIDR last
// changed, not when the user was last seen; nothing in Sphinx reads it.
type Record struct {
	CIDR      string    `json:"cidr"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store is the authoritative user document. Email is the map key, so a user has
// exactly one CIDR by construction.
type Store struct {
	Version    int               `json:"version"`
	Generation int64             `json:"generation"`
	Users      map[string]Record `json:"users"`
}

func newStore() *Store {
	return &Store{Version: storeVersion, Users: make(map[string]Record)}
}

// upsert sets email's CIDR and reports whether the store changed. Generation
// advances only on a real change, so a repeated authentication writes nothing.
func (s *Store) upsert(email, cidr string, now time.Time) bool {
	if existing, ok := s.Users[email]; ok && existing.CIDR == cidr {
		return false
	}
	s.Users[email] = Record{CIDR: cidr, UpdatedAt: now}
	s.Generation++
	return true
}

// CIDRs is the allowlist projection: every distinct CIDR, sorted for a stable
// comparison against what is already on the middleware.
func (s *Store) CIDRs() []string {
	set := make(map[string]struct{}, len(s.Users))
	for _, r := range s.Users {
		set[r.CIDR] = struct{}{}
	}
	cidrs := make([]string, 0, len(set))
	for c := range set {
		cidrs = append(cidrs, c)
	}
	sort.Strings(cidrs)
	return cidrs
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestStore' -v ./...`
Expected: PASS — all five subtests.

Then: `go vet ./... && gofmt -l . && go build ./...`
Expected: no output from any of them.

- [ ] **Step 5: Commit**

```bash
git add store.go store_internal_test.go
git commit -m "$(cat <<'EOF'
Add user store domain types keyed by email

Email is the map key, so one CIDR per user holds by construction.
Generation advances only when a record actually changes, which lets a
repeated authentication skip the write entirely.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: ConfigMap-backed UserStore with compare-and-swap

**Files:**
- Create: (append to) `store.go`
- Test: (append to) `store_internal_test.go`
- Modify: `go.mod`, `go.sum` (via `go mod tidy`)

**Interfaces:**
- Consumes: `Store`, `newStore`, `Store.upsert`, `storeKey`, `storeVersion`, `maxRetries` (Task 1); `configMapGVR` (existing, `main.go`).
- Produces:
  - `type UserStore interface { Load(ctx context.Context) (*Store, error); Upsert(ctx context.Context, email, cidr string, now time.Time) (*Store, error) }`
  - `func newConfigMapStore(client dynamic.Interface, namespace, name string) *configMapStore`
  - `func backoff(attempt int) time.Duration`
  - `func decodeStore(u *unstructured.Unstructured) (*Store, error)`

- [ ] **Step 1: Add the test helper and failing tests**

Append to `store_internal_test.go`. Note the helper — every later fake-client test reuses it.

```go
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
			// tracker, not c.Resource(...).Update(...): Fake.Invokes holds a
			// non-reentrant lock across the reactor callback, so a nested
			// client call deadlocks against the Update that dispatched us.
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
```

Add these imports to the top of `store_internal_test.go` (replacing the existing import block):

```go
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
```

- [ ] **Step 2: Tidy modules, then run the test to verify it fails**

Run: `go mod tidy && go test -run 'TestConfigMapStore' ./...`
Expected: `go mod tidy` adds indirect deps only (no new `require` block entries without `// indirect`). Test FAILs with build error `undefined: newConfigMapStore`.

- [ ] **Step 3: Write minimal implementation**

Append to `store.go`, and extend its import block to:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)
```

```go
// UserStore persists user records. Implementations are safe for concurrent use
// by multiple replicas; every mutation is a compare-and-swap.
type UserStore interface {
	Load(ctx context.Context) (*Store, error)
	Upsert(ctx context.Context, email, cidr string, now time.Time) (*Store, error)
}

type configMapStore struct {
	client    dynamic.Interface
	namespace string
	name      string
}

var _ UserStore = (*configMapStore)(nil)

func newConfigMapStore(client dynamic.Interface, namespace, name string) *configMapStore {
	return &configMapStore{client: client, namespace: namespace, name: name}
}

// backoff grows 25ms, 50ms, 100ms, 200ms, 400ms across the retry budget.
func backoff(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt)) * 25 * time.Millisecond
}

func (c *configMapStore) resource() dynamic.ResourceInterface {
	return c.client.Resource(configMapGVR).Namespace(c.namespace)
}

func (c *configMapStore) get(ctx context.Context) (*unstructured.Unstructured, error) {
	return c.resource().Get(ctx, c.name, metav1.GetOptions{})
}

// decodeStore reads users.json. A missing or empty key yields an empty store,
// which is what a fresh install looks like.
func decodeStore(u *unstructured.Unstructured) (*Store, error) {
	raw, found, err := unstructured.NestedString(u.Object, "data", storeKey)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", storeKey, err)
	}
	if !found || raw == "" {
		return newStore(), nil
	}
	var s Store
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", storeKey, err)
	}
	if s.Users == nil {
		s.Users = make(map[string]Record)
	}
	if s.Version == 0 {
		s.Version = storeVersion
	}
	return &s, nil
}

func (c *configMapStore) Load(ctx context.Context) (*Store, error) {
	u, err := c.get(ctx)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return newStore(), nil
		}
		return nil, fmt.Errorf("get configmap: %w", err)
	}
	return decodeStore(u)
}

// Check reports whether the ConfigMap is reachable, for the readiness probe.
// Absence is not a failure: a fresh install has no users yet.
func (c *configMapStore) Check(ctx context.Context) error {
	if _, err := c.get(ctx); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("get configmap: %w", err)
	}
	return nil
}

func (c *configMapStore) create(ctx context.Context, s *Store) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal store: %w", err)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": c.name, "namespace": c.namespace},
		"data":       map[string]any{storeKey: string(b)},
	}}
	_, err = c.resource().Create(ctx, u, metav1.CreateOptions{})
	return err
}

// write persists s into u, preserving u's resourceVersion so the API server
// rejects the update if another replica wrote first.
func (c *configMapStore) write(ctx context.Context, u *unstructured.Unstructured, s *Store) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal store: %w", err)
	}
	if err := unstructured.SetNestedField(u.Object, string(b), "data", storeKey); err != nil {
		return fmt.Errorf("set %s: %w", storeKey, err)
	}
	_, err = c.resource().Update(ctx, u, metav1.UpdateOptions{})
	return err
}

// Upsert sets email's CIDR under compare-and-swap, retrying on conflict. A
// no-op upsert returns the loaded store without writing.
func (c *configMapStore) Upsert(ctx context.Context, email, cidr string, now time.Time) (*Store, error) {
	var last error
	for attempt := range maxRetries {
		u, err := c.get(ctx)
		missing := k8serrors.IsNotFound(err)
		if err != nil && !missing {
			return nil, fmt.Errorf("get configmap: %w", err)
		}

		s := newStore()
		if !missing {
			if s, err = decodeStore(u); err != nil {
				return nil, err
			}
		}
		if !s.upsert(email, cidr, now) {
			return s, nil
		}

		if missing {
			err = c.create(ctx, s)
			if err == nil {
				return s, nil
			}
			if !k8serrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("create configmap: %w", err)
			}
		} else {
			err = c.write(ctx, u, s)
			if err == nil {
				return s, nil
			}
			if !k8serrors.IsConflict(err) {
				return nil, fmt.Errorf("update configmap: %w", err)
			}
		}
		last = err
		// No point sleeping after the final attempt, and a cancelled caller
		// must not wait out a backoff it will never use.
		if attempt == maxRetries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return nil, fmt.Errorf("upsert %s: exceeded %d retries: %w", email, maxRetries, last)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestStore|TestConfigMapStore' -v ./...`
Expected: PASS, including `retries on conflict and re-reads` and `gives up after maxRetries conflicts`.

Then: `go vet ./... && gofmt -l . && go build ./...`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add store.go store_internal_test.go go.mod go.sum
git commit -m "$(cat <<'EOF'
Add ConfigMap-backed user store with compare-and-swap

One shared JSON document keyed by email, replacing the per-pod sharded
keys. Writes preserve resourceVersion and retry on conflict, re-reading
each attempt so a competing replica's record is never clobbered.

The fake dynamic client does not track resourceVersion, so the conflict
path is exercised by injecting a 409 through a reactor.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Migration off the pod-sharded format

**Files:**
- Modify: (append to) `store.go`
- Test: (append to) `store_internal_test.go`

**Interfaces:**
- Consumes: `configMapStore`, `decodeStore`, `newStore`, `storeKey` (Tasks 1–2); `hostCIDR` (existing, `main.go:175`).
- Produces:
  - `type legacyUser struct { Email string; IP string }`
  - `func (c *configMapStore) Migrate(ctx context.Context, now time.Time) error`

Legacy layout, for reference — each key is a pod hostname, each value a JSON `map[email]user` where `user` is `{"email": ..., "ip": ...}` holding a **bare IP**, not a CIDR:

```yaml
data:
  sphinx-abc123: '{"alice@example.com":{"email":"alice@example.com","ip":"203.0.113.7"}}'
  sphinx-def456: '{"bob@example.com":{"email":"bob@example.com","ip":"198.51.100.4"}}'
```

- [ ] **Step 1: Write the failing test**

Append to `store_internal_test.go`:

```go
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
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestConfigMapStoreMigrate' ./...`
Expected: FAIL, build error `undefined: legacyUser`

- [ ] **Step 3: Write minimal implementation**

Append to `store.go`. Add `"log"` to its imports.

```go
// legacyUser is the record shape of the retired pod-sharded format. It holds a
// bare IP, not a CIDR.
type legacyUser struct {
	Email string `json:"email"`
	IP    string `json:"ip"`
}

// Migrate rewrites the retired hostname-keyed layout into a single users.json
// document, dropping the legacy keys in the same atomic update. It is a no-op
// once users.json exists, or when the ConfigMap is absent.
//
// A user whose pod blobs disagree about their IP is dropped: the legacy format
// carries no timestamp, so the current CIDR cannot be determined, and keeping
// the wrong one would preserve exactly the staleness this redesign removes.
func (c *configMapStore) Migrate(ctx context.Context, now time.Time) error {
	u, err := c.get(ctx)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get configmap: %w", err)
	}

	data, found, err := unstructured.NestedStringMap(u.Object, "data")
	if err != nil {
		return fmt.Errorf("read data: %w", err)
	}
	if !found || len(data) == 0 {
		return nil
	}
	if _, ok := data[storeKey]; ok {
		return nil
	}

	seen := make(map[string]map[string]struct{})
	for key, raw := range data {
		var blob map[string]legacyUser
		if err := json.Unmarshal([]byte(raw), &blob); err != nil {
			log.Printf("Migration: skipping unparseable legacy key %q: %v", key, err)
			continue
		}
		for email, lu := range blob {
			cidr, err := hostCIDR(lu.IP)
			if err != nil {
				log.Printf("Migration: skipping user %s with unparseable ip %q: %v", email, lu.IP, err)
				continue
			}
			if seen[email] == nil {
				seen[email] = make(map[string]struct{})
			}
			seen[email][cidr] = struct{}{}
		}
	}

	s := newStore()
	for email, cidrs := range seen {
		if len(cidrs) != 1 {
			log.Printf("Migration: dropping user %s: %d conflicting CIDRs across legacy keys, re-authentication required", email, len(cidrs))
			continue
		}
		for cidr := range cidrs {
			s.Users[email] = Record{CIDR: cidr, UpdatedAt: now}
		}
	}
	s.Generation = 1

	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal store: %w", err)
	}
	// Replace data wholesale: writes users.json and removes every legacy key in
	// one update, which is atomic because it is a single object.
	if err := unstructured.SetNestedStringMap(u.Object, map[string]string{storeKey: string(b)}, "data"); err != nil {
		return fmt.Errorf("set data: %w", err)
	}
	if _, err := c.resource().Update(ctx, u, metav1.UpdateOptions{}); err != nil {
		// Losing the migration race is the expected outcome for every replica
		// but one. It means the work is already done, not that startup failed.
		if k8serrors.IsConflict(err) && c.migrated(ctx) {
			log.Printf("Migration: another replica migrated first")
			return nil
		}
		return fmt.Errorf("update configmap: %w", err)
	}
	log.Printf("Migration: wrote %d users, removed %d legacy keys", len(s.Users), len(data))
	return nil
}

// migrated reports whether users.json now exists, meaning another replica won
// the migration race.
func (c *configMapStore) migrated(ctx context.Context) bool {
	u, err := c.get(ctx)
	if err != nil {
		return false
	}
	data, found, err := unstructured.NestedStringMap(u.Object, "data")
	if err != nil || !found {
		return false
	}
	_, ok := data[storeKey]
	return ok
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestConfigMapStoreMigrate' -v ./...`
Expected: PASS — all six subtests.

Then: `go test ./... && go vet ./... && gofmt -l . && go build ./...`
Expected: `ok`, then no output.

- [ ] **Step 5: Commit**

```bash
git add store.go store_internal_test.go
git commit -m "$(cat <<'EOF'
Migrate the pod-sharded ConfigMap layout to a single document

Merges hostname-keyed blobs into users.json and drops the legacy keys in
one atomic update. Users whose blobs disagree about their IP are dropped
rather than guessed at: the legacy format has no timestamps, so keeping
either CIDR risks preserving the stale one.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Traefik allowlist writer

The task that kills the bug: `Apply` replaces `sourceRange` instead of unioning into it.

**Files:**
- Create: `allowlist.go`
- Test: `allowlist_internal_test.go`

**Interfaces:**
- Consumes: `maxRetries`, `backoff` (Tasks 1–2); `middlewareGVR` (existing, `main.go`).
- Produces:
  - `const generationAnnotation = "sphinx.bartlett.ops/generation"`
  - `type Allowlist interface { Apply(ctx context.Context, cidrs []string, generation int64) error }`
  - `func newTraefikAllowlist(client dynamic.Interface, namespace, name string) *traefikAllowlist`
  - `func (a *traefikAllowlist) EnsureExists(ctx context.Context) error`
  - `func stampedGeneration(u *unstructured.Unstructured) int64`

- [ ] **Step 1: Write the failing test**

Create `allowlist_internal_test.go`:

```go
package main

import (
	"context"
	"errors"
	"testing"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"
)

func middlewareWith(t *testing.T, sourceRange []string, annotations map[string]string) *unstructured.Unstructured {
	t.Helper()
	sr := make([]any, 0, len(sourceRange))
	for _, c := range sourceRange {
		sr = append(sr, c)
	}
	meta := map[string]any{"name": "sphinx-allowlist", "namespace": "kube-system"}
	if len(annotations) > 0 {
		a := make(map[string]any, len(annotations))
		for k, v := range annotations {
			a[k] = v
		}
		meta["annotations"] = a
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "Middleware",
		"metadata":   meta,
		"spec":       map[string]any{"ipAllowList": map[string]any{"sourceRange": sr}},
	}}
}

func readSourceRange(t *testing.T, c dynamic.Interface) []string {
	t.Helper()
	u, err := c.Resource(middlewareGVR).Namespace("kube-system").
		Get(context.Background(), "sphinx-allowlist", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get middleware: %v", err)
	}
	sr, _, err := unstructured.NestedStringSlice(u.Object, "spec", "ipAllowList", "sourceRange")
	if err != nil {
		t.Fatalf("read sourceRange: %v", err)
	}
	return sr
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The single assertion that would have caught the reported bug. unionStrings
// fails it.
func TestAllowlistApplyReplacesRatherThanUnions(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t, middlewareWith(t, []string{"203.0.113.7/32"}, nil))
	a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

	if err := a.Apply(ctx, []string{"198.51.100.4/32"}, 1); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readSourceRange(t, c)
	if !equalStrings(got, []string{"198.51.100.4/32"}) {
		t.Errorf("sourceRange = %v, want exactly [198.51.100.4/32]; the stale CIDR must be gone", got)
	}
}

func TestAllowlistGenerationGuard(t *testing.T) {
	ctx := context.Background()

	t.Run("older generation is a no-op", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t,
			[]string{"198.51.100.4/32"},
			map[string]string{generationAnnotation: "43"}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 42); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"198.51.100.4/32"}) {
			t.Errorf("sourceRange = %v, want the newer generation's value to survive", got)
		}
	})

	// Equal generations must proceed, or the reconcile loop is inert.
	t.Run("equal generation proceeds and repairs drift", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t,
			[]string{"1.2.3.4/32"}, // hand-edited junk
			map[string]string{generationAnnotation: "42"}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 42); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want drift repaired to [203.0.113.7/32]", got)
		}
	})

	t.Run("missing annotation proceeds", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, []string{"1.2.3.4/32"}, nil))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 7); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want [203.0.113.7/32]", got)
		}
	})

	t.Run("malformed annotation is treated as zero", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t,
			[]string{"1.2.3.4/32"},
			map[string]string{generationAnnotation: "banana"}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 1); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want the write to proceed", got)
		}
	})

	t.Run("stamps the generation it applied", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, nil, nil))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 42); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		u, err := c.Resource(middlewareGVR).Namespace("kube-system").
			Get(ctx, "sphinx-allowlist", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get middleware: %v", err)
		}
		if got := stampedGeneration(u); got != 42 {
			t.Errorf("stamped generation = %d, want 42", got)
		}
	})
}

func TestAllowlistApplyEmptySet(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t, middlewareWith(t, []string{"203.0.113.7/32"}, nil))
	a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

	if err := a.Apply(ctx, nil, 1); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readSourceRange(t, c); len(got) != 0 {
		t.Errorf("sourceRange = %v, want empty when no users remain", got)
	}
}

func TestAllowlistApplyRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t, middlewareWith(t, nil, nil))

	var fired bool
	c.PrependReactor("update", "middlewares", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fired {
			return false, nil, nil
		}
		fired = true
		return true, nil, k8serrors.NewConflict(
			schema.GroupResource{Resource: "middlewares"}, "sphinx-allowlist", errors.New("stale"))
	})

	a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")
	if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 1); err != nil {
		t.Fatalf("Apply should recover from a conflict: %v", err)
	}
	if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
		t.Errorf("sourceRange = %v, want [203.0.113.7/32]", got)
	}
}

func TestAllowlistEnsureExists(t *testing.T) {
	ctx := context.Background()

	t.Run("creates when absent", func(t *testing.T) {
		c := newFakeClient(t)
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")
		if err := a.EnsureExists(ctx); err != nil {
			t.Fatalf("EnsureExists: %v", err)
		}
		if _, err := c.Resource(middlewareGVR).Namespace("kube-system").
			Get(ctx, "sphinx-allowlist", metav1.GetOptions{}); err != nil {
			t.Fatalf("middleware should exist: %v", err)
		}
	})

	t.Run("leaves an existing middleware untouched", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, []string{"203.0.113.7/32"}, nil))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")
		if err := a.EnsureExists(ctx); err != nil {
			t.Fatalf("EnsureExists: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want untouched", got)
		}
	})

	// Replicas starting together race to create the middleware. Losing that
	// race is success, not a startup failure.
	t.Run("losing the create race is not an error", func(t *testing.T) {
		c := newFakeClient(t)
		c.PrependReactor("create", "middlewares", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, k8serrors.NewAlreadyExists(
				schema.GroupResource{Group: "traefik.io", Resource: "middlewares"}, "sphinx-allowlist")
		})
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.EnsureExists(ctx); err != nil {
			t.Fatalf("losing the create race must not be an error: %v", err)
		}
	})

	t.Run("a non-conflict create failure surfaces", func(t *testing.T) {
		c := newFakeClient(t)
		c.PrependReactor("create", "middlewares", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, k8serrors.NewForbidden(
				schema.GroupResource{Group: "traefik.io", Resource: "middlewares"}, "sphinx-allowlist",
				errors.New("rbac denied"))
		})
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.EnsureExists(ctx); err == nil {
			t.Fatal("a Forbidden create must surface as an error")
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestAllowlist' ./...`
Expected: FAIL, build error `undefined: newTraefikAllowlist`

- [ ] **Step 3: Write minimal implementation**

Create `allowlist.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// generationAnnotation records the store generation last written to the
// middleware, so a slow replica cannot overwrite a newer allowlist.
const generationAnnotation = "sphinx.bartlett.ops/generation"

// Allowlist writes the desired CIDR set. Implementations know nothing about
// users; the set is already projected.
type Allowlist interface {
	Apply(ctx context.Context, cidrs []string, generation int64) error
}

type traefikAllowlist struct {
	client    dynamic.Interface
	namespace string
	name      string
}

var _ Allowlist = (*traefikAllowlist)(nil)

func newTraefikAllowlist(client dynamic.Interface, namespace, name string) *traefikAllowlist {
	return &traefikAllowlist{client: client, namespace: namespace, name: name}
}

func (a *traefikAllowlist) resource() dynamic.ResourceInterface {
	return a.client.Resource(middlewareGVR).Namespace(a.namespace)
}

// EnsureExists creates the Middleware when it is absent.
func (a *traefikAllowlist) EnsureExists(ctx context.Context) error {
	_, err := a.resource().Get(ctx, a.name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("get middleware: %w", err)
	}

	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "Middleware",
		"metadata":   map[string]any{"name": a.name, "namespace": a.namespace},
		"spec":       map[string]any{"ipAllowList": map[string]any{"sourceRange": []any{}}},
	}}
	if _, err := a.resource().Create(ctx, u, metav1.CreateOptions{}); err != nil {
		// Replicas starting together race to create it. Losing means the
		// middleware exists, which is all EnsureExists promises.
		if k8serrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create middleware: %w", err)
	}
	log.Printf("Created middleware %s/%s", a.namespace, a.name)
	return nil
}

// Check reports whether the Middleware is reachable, for the readiness probe.
// Unlike the ConfigMap, it must exist: EnsureExists created it at startup.
func (a *traefikAllowlist) Check(ctx context.Context) error {
	if _, err := a.resource().Get(ctx, a.name, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("get middleware: %w", err)
	}
	return nil
}

// stampedGeneration reads the generation last applied. An absent or malformed
// annotation reads as 0, so the next Apply always proceeds.
func stampedGeneration(u *unstructured.Unstructured) int64 {
	raw, found, err := unstructured.NestedString(u.Object, "metadata", "annotations", generationAnnotation)
	if err != nil || !found || raw == "" {
		return 0
	}
	g, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		log.Printf("Middleware has malformed %s annotation %q, treating as 0", generationAnnotation, raw)
		return 0
	}
	return g
}

// Apply replaces spec.ipAllowList.sourceRange with exactly cidrs. It never
// unions: that is what let a re-authenticating user's old CIDR survive.
//
// A write whose generation is strictly older than the stamped one is skipped,
// so a late replica cannot resurrect a removed CIDR. An equal generation
// proceeds, which is how the background reconcile repairs drift.
func (a *traefikAllowlist) Apply(ctx context.Context, cidrs []string, generation int64) error {
	if cidrs == nil {
		cidrs = []string{}
	}
	var last error
	for attempt := range maxRetries {
		u, err := a.resource().Get(ctx, a.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get middleware: %w", err)
		}

		if stamped := stampedGeneration(u); generation < stamped {
			log.Printf("Skipping allowlist write: generation %d is older than stamped %d", generation, stamped)
			return nil
		}

		if err := unstructured.SetNestedStringSlice(u.Object, cidrs, "spec", "ipAllowList", "sourceRange"); err != nil {
			return fmt.Errorf("set sourceRange: %w", err)
		}
		if err := unstructured.SetNestedField(u.Object, strconv.FormatInt(generation, 10),
			"metadata", "annotations", generationAnnotation); err != nil {
			return fmt.Errorf("set generation annotation: %w", err)
		}

		if _, err = a.resource().Update(ctx, u, metav1.UpdateOptions{}); err == nil {
			return nil
		}
		if !k8serrors.IsConflict(err) {
			return fmt.Errorf("update middleware: %w", err)
		}
		last = err
		// No point sleeping after the final attempt, and a cancelled caller
		// must not wait out a backoff it will never use.
		if attempt == maxRetries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return fmt.Errorf("apply allowlist: exceeded %d retries: %w", maxRetries, last)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestAllowlist' -v ./...`
Expected: PASS, notably `TestAllowlistApplyReplacesRatherThanUnions` and `equal generation proceeds and repairs drift`.

Then: `go test ./... && go vet ./... && gofmt -l . && go build ./...`
Expected: `ok`, then no output.

- [ ] **Step 5: Commit**

```bash
git add allowlist.go allowlist_internal_test.go
git commit -m "$(cat <<'EOF'
Add Traefik allowlist writer that replaces sourceRange

Apply writes exactly the projected CIDR set instead of unioning into the
existing one, so a removed CIDR actually disappears from the middleware.

A generation annotation guards against a slow replica overwriting a newer
allowlist. The comparison is strictly-older rather than not-newer, so an
equal-generation re-apply still repairs hand-edited drift; rejecting it
would make the reconcile loop inert.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Reconciler with write-skip cache

**Files:**
- Create: `reconciler.go`
- Test: `reconciler_internal_test.go`

**Interfaces:**
- Consumes: `UserStore`, `Store`, `newStore`, `Store.upsert`, `Store.CIDRs` (Tasks 1–2); `Allowlist` (Task 4).
- Produces:
  - `func newReconciler(s UserStore, a Allowlist) *Reconciler`
  - `func (r *Reconciler) Authenticate(ctx context.Context, email, cidr string) (bool, error)` — reports whether a write occurred
  - `func (r *Reconciler) Reconcile(ctx context.Context) error`
  - `func (r *Reconciler) Healthy(now time.Time, maxAge time.Duration) bool`
  - `func (r *Reconciler) Run(ctx context.Context, interval time.Duration)`

**Design note for the implementer.** `Reconcile` rebuilds the write-skip cache from the freshly loaded store. Without this, a pod's cache could keep short-circuiting on a CIDR that another replica has since replaced, returning 200 to a user who is no longer in the allowlist. The cache is a cache, never a source of truth, and this is what keeps that true.

- [ ] **Step 1: Write the failing test**

Create `reconciler_internal_test.go`:

```go
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
	err     error
}

func (f *fakeAllowlist) Apply(_ context.Context, cidrs []string, generation int64) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.last = append([]string(nil), cidrs...)
	f.lastGen = generation
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestReconcile' ./...`
Expected: FAIL, build error `undefined: newReconciler`

- [ ] **Step 3: Write minimal implementation**

Create `reconciler.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Reconciler projects the user store onto the allowlist. Both the auth path and
// the background ticker funnel through project, so the middleware is always a
// derived view of the store and never an accumulator.
type Reconciler struct {
	store     UserStore
	allowlist Allowlist

	// cache short-circuits the hot path. ForwardAuth calls /auth on every
	// request, so an unchanged CIDR must not reach the API server. It is a
	// cache, never a source of truth: Reconcile rebuilds it from the store.
	mu    sync.Mutex
	cache map[string]string

	healthMu       sync.RWMutex
	lastReconcile  time.Time
	reconciledOnce bool
}

func newReconciler(s UserStore, a Allowlist) *Reconciler {
	return &Reconciler{store: s, allowlist: a, cache: make(map[string]string)}
}

func (r *Reconciler) project(ctx context.Context, s *Store) error {
	return r.allowlist.Apply(ctx, s.CIDRs(), s.Generation)
}

// Authenticate records email at cidr and reports whether a write occurred; a
// false result means the request was served from the local cache. The allowlist
// is updated before returning, so a success never precedes the user being
// allowlisted.
func (r *Reconciler) Authenticate(ctx context.Context, email, cidr string) (bool, error) {
	r.mu.Lock()
	cached, ok := r.cache[email]
	r.mu.Unlock()
	if ok && cached == cidr {
		return false, nil
	}

	s, err := r.store.Upsert(ctx, email, cidr, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("upsert user: %w", err)
	}
	if err := r.project(ctx, s); err != nil {
		return false, fmt.Errorf("apply allowlist: %w", err)
	}

	r.mu.Lock()
	r.cache[email] = cidr
	r.mu.Unlock()
	return true, nil
}

// Reconcile re-projects the store onto the allowlist, repairing drift, and
// rebuilds the cache so a stale entry cannot mask a CIDR another replica
// changed.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	s, err := r.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load store: %w", err)
	}
	if err := r.project(ctx, s); err != nil {
		return fmt.Errorf("apply allowlist: %w", err)
	}

	fresh := make(map[string]string, len(s.Users))
	for email, rec := range s.Users {
		fresh[email] = rec.CIDR
	}
	r.mu.Lock()
	r.cache = fresh
	r.mu.Unlock()

	r.healthMu.Lock()
	r.lastReconcile = time.Now()
	r.reconciledOnce = true
	r.healthMu.Unlock()
	return nil
}

// Healthy reports whether a reconcile has succeeded within maxAge. A pod that
// has silently lost the ability to write the allowlist falls out of the Service.
func (r *Reconciler) Healthy(now time.Time, maxAge time.Duration) bool {
	r.healthMu.RLock()
	defer r.healthMu.RUnlock()
	return r.reconciledOnce && now.Sub(r.lastReconcile) <= maxAge
}

// Run reconciles on every tick until ctx is cancelled. Errors are logged and
// swallowed: a transient API failure must not take down the auth path.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Reconcile(ctx); err != nil {
				log.Printf("Reconcile failed: %v", err)
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestReconcil' -v ./...`
Expected: PASS — every subtest, notably `TestReconcilerReAuthReplacesCIDR`.

Then: `go test ./... && go vet ./... && gofmt -l . && go build ./...`
Expected: `ok`, then no output.

- [ ] **Step 5: Commit**

```bash
git add reconciler.go reconciler_internal_test.go
git commit -m "$(cat <<'EOF'
Add reconciler projecting the user store onto the allowlist

Authenticate upserts then applies, so a 201 never precedes the user being
allowlisted. A per-pod cache short-circuits the ForwardAuth hot path when
the CIDR is unchanged.

Reconcile repairs drift on a ticker and rebuilds the cache from the store,
so a stale entry cannot short-circuit a CIDR another replica has replaced.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Rewire main.go and delete middleware.go

The first task where behaviour changes in the running binary. Everything before this was additive.

**Files:**
- Modify: `main.go`
- Delete: `middleware.go`
- Test: `main_internal_test.go` (unchanged — `TestHostCIDR` must still pass)

**Interfaces:**
- Consumes: `newConfigMapStore`, `configMapStore.Migrate` (Tasks 2–3); `newTraefikAllowlist`, `traefikAllowlist.EnsureExists` (Task 4); `newReconciler`, `Reconciler.Authenticate`, `Reconciler.Reconcile`, `Reconciler.Healthy`, `Reconciler.Run` (Task 5).
- Produces: nothing consumed by later tasks.

**Note on readiness.** The old probe issued its own `Get` against the middleware and ConfigMap. Those checks are now redundant: `Reconcile` reads the ConfigMap and writes the middleware, so if either is unreachable the reconcile fails and `Healthy` goes false. The probe checks `Healthy` alone, which is strictly stronger than the two `Get` calls it replaces.

- [ ] **Step 1: Delete middleware.go and rewrite main.go**

```bash
git rm middleware.go
```

Replace the entire contents of `main.go` with:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/peterbourgon/ff/v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	middlewareGVR = schema.GroupVersionResource{
		Group:    "traefik.io",
		Version:  "v1alpha1",
		Resource: "middlewares",
	}
	configMapGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "configmaps",
	}
)

func main() {
	port := flag.Int("port", 8080, "Port to run server on")
	trustedProxiesRaw := flag.String("trusted-proxies", "", "Comma separated list of trusted proxies in CIDR format")
	middlewareName := flag.String("middleware-name", "", "Name of allowlist middleware")
	middlewareNamespace := flag.String("middleware-namespace", "kube-system", "Namespace of middleware")
	configMapName := flag.String("configmap-name", "sphinx-users", "Name of ConfigMap for user persistence")
	reconcileInterval := flag.Duration("reconcile-interval", 60*time.Second, "Background drift-repair period")
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig file (auto-detected if not set)")
	if err := ff.Parse(flag.CommandLine, os.Args[1:], ff.WithEnvVarPrefix("SPHINX")); err != nil {
		log.Fatal(err)
	}

	if *middlewareName == "" {
		log.Fatal("middleware-name not set")
	}
	if *reconcileInterval <= 0 {
		log.Fatal("reconcile-interval must be positive")
	}

	var trustedProxies []string
	if *trustedProxiesRaw != "" {
		trustedProxies = strings.Split(*trustedProxiesRaw, ",")
	}

	cfg, err := resolveKubeConfig(*kubeconfig)
	if err != nil {
		log.Fatal(err)
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	allowlist := newTraefikAllowlist(client, *middlewareNamespace, *middlewareName)
	if err := allowlist.EnsureExists(ctx); err != nil {
		log.Fatal(err)
	}

	store := newConfigMapStore(client, *middlewareNamespace, *configMapName)
	if err := store.Migrate(ctx, time.Now().UTC()); err != nil {
		log.Fatalf("migrate store: %v", err)
	}

	reconciler := newReconciler(store, allowlist)
	// The initial reconcile prunes whatever stale CIDRs the retired
	// append-only middleware accumulated.
	if err := reconciler.Reconcile(ctx); err != nil {
		log.Fatalf("initial reconcile: %v", err)
	}
	go reconciler.Run(ctx, *reconcileInterval)

	router := gin.New()
	router.Use(gin.LoggerWithConfig(gin.LoggerConfig{SkipPaths: []string{"/health", "/ready"}}))
	router.Use(gin.Recovery())
	if err := router.SetTrustedProxies(trustedProxies); err != nil {
		log.Fatal(err)
	}
	router.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.GET("/ready", readiness(reconciler, allowlist, store, *reconcileInterval))
	router.GET("/users", getUsers(store))
	router.POST("/users", auth(reconciler)) // Backwards compatibility
	router.GET("/auth", auth(reconciler))   // Backwards compatibility

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: router}
	go func() {
		<-ctx.Done()
		// SIGTERM: stop accepting, drain in-flight requests. Blocking in
		// router.Run instead would serve auth traffic against a frozen
		// reconciler until the kubelet killed us.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("Graceful shutdown failed: %v", err)
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// checker reports whether a dependency is reachable.
type checker interface {
	Check(ctx context.Context) error
}

// readiness requires both a recent successful reconcile and reachable
// dependencies. The freshness check does not subsume the live reads: Healthy
// reports on the last *successful* reconcile, so a pod whose API access breaks
// right after one keeps reporting ready for up to 3 intervals. Together they
// are strictly stronger than the retired probe; neither is alone.
func readiness(r *Reconciler, allowlist, store checker, interval time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !r.Healthy(time.Now(), 3*interval) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no successful reconcile within 3 intervals"})
			return
		}
		ctx := c.Request.Context()
		if err := allowlist.Check(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("middleware unavailable: %v", err)})
			return
		}
		if err := store.Check(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("configmap unavailable: %v", err)})
			return
		}
		c.Status(http.StatusOK)
	}
}

func resolveKubeConfig(override string) (*rest.Config, error) {
	if override != "" {
		return clientcmd.BuildConfigFromFlags("", override)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}
	return clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
}

func resolveClientIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i != -1 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return c.ClientIP()
}

// hostCIDR converts a bare IP address into the CIDR covering only that host:
// /32 for IPv4, /128 for IPv6.
func hostCIDR(ip string) (string, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("parse ip: %w", err)
	}
	// An IPv4 address received over a v6 socket parses as ::ffff:a.b.c.d, whose
	// BitLen is 128. Unmapping keeps it a /32 rather than widening it to a /128.
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()).String(), nil
}

func getUsers(store UserStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		s, err := store.Load(c.Request.Context())
		if err != nil {
			log.Printf("Failed to load users: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load users"})
			return
		}
		c.IndentedJSON(http.StatusOK, s.Users)
	}
}

func auth(r *Reconciler) gin.HandlerFunc {
	return func(c *gin.Context) {
		email := c.GetHeader("X-Forwarded-User")
		if email == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Missing X-Forwarded-User header"})
			return
		}
		cidr, err := hostCIDR(resolveClientIP(c))
		if err != nil {
			log.Printf("Failed to resolve client ip for %s: %v", email, err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Unresolvable client IP"})
			return
		}

		wrote, err := r.Authenticate(c.Request.Context(), email, cidr)
		if err != nil {
			log.Printf("Failed to add user %s: %v", email, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add user"})
			return
		}

		status := http.StatusOK
		if wrote {
			status = http.StatusCreated
			log.Printf("Registered %s at %s", email, cidr)
		}
		c.IndentedJSON(status, gin.H{"email": email, "cidr": cidr})
	}
}
```

- [ ] **Step 2: Verify the whole suite still passes**

Run: `go build ./... && go test ./... && go vet ./... && gofmt -l .`
Expected: `build ok`, `ok bartlett-ops/sphinx`, no vet output, no gofmt output.

`TestHostCIDR` in `main_internal_test.go` must still pass unchanged.

- [ ] **Step 3: Confirm the retired symbols are gone**

Run:
```bash
grep -rnE 'instanceID|usersMu|unionStrings|getCIDRsFromUsers|saveUsers|loadUsers|getUnstructured' --include='*.go' .
```
Expected: **no output.** Every one of these is part of the old design. If any remain, remove them.

- [ ] **Step 4: Confirm the new flag parses from the environment**

Run: `SPHINX_RECONCILE_INTERVAL=30s go run . --middleware-name x --kubeconfig /nonexistent 2>&1 | head -3`
Expected: it fails on the kubeconfig, **not** on flag parsing. A message mentioning `/nonexistent` or `stat` proves `--reconcile-interval` accepted `30s` from the environment.

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "$(cat <<'EOF'
Rewire main.go onto the store, allowlist, and reconciler

Deletes middleware.go, along with the instanceID pod sharding, the global
users map, and the append-only unionStrings that let stale CIDRs survive.

Handlers close over the reconciler instead of package globals. The auth
handler now returns 201 only when a write occurred and 200 on a cache hit.
Readiness requires a recent successful reconcile, which subsumes probing
the middleware and ConfigMap directly.

Adds --reconcile-interval (default 60s).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Update README and RBAC

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: the deployed behaviour from Task 6.
- Produces: nothing.

- [ ] **Step 1: Fix the multi-replica paragraph**

In `README.md`, replace this line (currently line 12):

```markdown
Multiple Sphinx replicas co-exist safely — each instance owns its own key in the `ConfigMap` (keyed by pod hostname) using Kubernetes Server-Side Apply, so replicas never overwrite each other.
```

with:

```markdown
Multiple Sphinx replicas co-exist safely. All replicas share a single `ConfigMap` document keyed by user email, mutated by compare-and-swap on `resourceVersion` and retried on conflict. Each user has exactly one CIDR: re-authenticating from a new address replaces the old one, and the Traefik allowlist is rewritten to exactly the set of registered CIDRs rather than accumulated. Records persist indefinitely; there is no expiry.
```

- [ ] **Step 2: Fix the numbered registration flow**

Replace step 3 and add step 5 so the list reads:

```markdown
1. Reads the `X-Forwarded-User` header (set by your identity provider / auth proxy).
2. Converts the client IP to a single-host CIDR (`/32` for IPv4, `/128` for IPv6).
3. Records the CIDR against the email in a Kubernetes `ConfigMap`, replacing any CIDR previously held for that user.
4. Rewrites the Traefik `Middleware` `ipAllowList` to exactly the set of registered CIDRs.
5. Re-projects the allowlist on a timer (`--reconcile-interval`) to repair drift.
```

Note the header name: the code reads `X-Forwarded-User`, while README steps 1 and the API section still say `X-User-Email`. Correct both to `X-Forwarded-User`.

- [ ] **Step 3: Add the new flag to the configuration table**

Insert into the flag table, after the `--configmap-name` row:

```markdown
| `--reconcile-interval`  | `SPHINX_RECONCILE_INTERVAL`   | `60s`              | No       | How often the allowlist is re-projected from the store to repair drift.     |
```

- [ ] **Step 4: Fix the RBAC list**

Replace the RBAC bullet list at the end of the file with:

```markdown
- `get`, `create`, `update` on `middlewares.traefik.io`
- `get`, `create`, `update` on `configmaps`
```

Server-Side Apply is gone, so `patch` is no longer required; `create` is now needed because the store creates the `ConfigMap` on first write.

- [ ] **Step 5: Verify and commit**

Run: `grep -n 'X-User-Email\|Server-Side Apply\|pod hostname\|patch' README.md`
Expected: **no output.** Each is a remnant of the retired design.

```bash
git add README.md
git commit -m "$(cat <<'EOF'
Update README for the email-keyed store

Documents the shared compare-and-swap document, one CIDR per user, and
--reconcile-interval. Corrects RBAC: configmaps need create+update rather
than patch, now that Server-Side Apply is gone.

Also corrects the auth header name, which the docs still called
X-User-Email after the code moved to X-Forwarded-User.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: Verify against a live cluster

Unit tests cannot prove Traefik accepts what Sphinx writes. Two things in this design have never touched a real API server: an empty `sourceRange` array, and the annotation on the `Middleware`.

**Files:** none — this task changes no code unless a defect is found.

- [ ] **Step 1: Confirm the CRD accepts an empty sourceRange**

With a cluster reachable and the Traefik CRDs installed:

```bash
kubectl apply --dry-run=server -f - <<'EOF'
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: sphinx-allowlist-probe
  namespace: kube-system
  annotations:
    sphinx.bartlett.ops/generation: "0"
spec:
  ipAllowList:
    sourceRange: []
EOF
```

Expected: `middleware.traefik.io/sphinx-allowlist-probe created (server dry run)`.

**Resolved 2026-07-09 against a live cluster (Traefik CRDs installed):** the CRD accepts
both an empty `sourceRange: []` and the `sphinx.bartlett.ops/generation` annotation. The
dry run persisted nothing. No code change is needed; the fallback below was not required.

If the CRD **rejects** an empty array (a `minItems` validation error), stop and report. The fix is for `Apply` to remove the `sourceRange` field when `cidrs` is empty, via `unstructured.RemoveNestedField(u.Object, "spec", "ipAllowList", "sourceRange")`, and a matching change to `TestAllowlistApplyEmptySet`. Do not guess — the empty case is what a fresh install with no users produces.

- [ ] **Step 2: Exercise the real registration path**

Run Sphinx against the cluster:

```bash
go run . --middleware-name sphinx-allowlist --middleware-namespace kube-system --trusted-proxies 0.0.0.0/0
```

In a second shell, register a user twice from different addresses:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-User: alice@example.com' -H 'X-Forwarded-For: 203.0.113.7' localhost:8080/auth
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-User: alice@example.com' -H 'X-Forwarded-For: 203.0.113.7' localhost:8080/auth
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-User: alice@example.com' -H 'X-Forwarded-For: 198.51.100.4' localhost:8080/auth
```

Expected: `200`, then `200`, then `201`.

The first call returns `200`, not `201`, and that is correct. The startup `Reconcile`
rebuilds the write-skip cache from the store, so a user already registered at that CIDR is
a cache hit and no write occurs. `201 Created` is reserved for a registration that was
actually written. Only the third call, from a new address, writes.

- [ ] **Step 3: Confirm the old CIDR is gone — the whole point**

```bash
kubectl get middleware sphinx-allowlist -n kube-system -o jsonpath='{.spec.ipAllowList.sourceRange}'; echo
kubectl get cm sphinx-users -n kube-system -o jsonpath='{.data.users\.json}'; echo
```

Expected: `sourceRange` contains `198.51.100.4/32` and **not** `203.0.113.7/32`. The ConfigMap holds exactly one record for `alice@example.com`.

- [ ] **Step 4: Confirm drift repair**

Hand-edit the middleware, then wait one reconcile interval:

```bash
kubectl patch middleware sphinx-allowlist -n kube-system --type=merge \
  -p '{"spec":{"ipAllowList":{"sourceRange":["1.2.3.4/32"]}}}'
sleep 65
kubectl get middleware sphinx-allowlist -n kube-system -o jsonpath='{.spec.ipAllowList.sourceRange}'; echo
```

Expected: `198.51.100.4/32` is restored and `1.2.3.4/32` is gone. This proves the equal-generation `Apply` is not inert — the reconcile loop re-applied generation *n* over hand-edited drift.

- [ ] **Step 5: Report**

No commit unless Step 1 forced a code change. Report the observed output of each step. If Step 4 leaves `1.2.3.4/32` in place, the generation guard is rejecting equal generations and the reconcile loop is inert — a real defect, not a test artifact.

---

## Self-Review

**Spec coverage.** Every section of `docs/superpowers/specs/2026-07-09-user-cidr-storage-design.md` maps to a task:

| Spec section | Task |
|---|---|
| Guiding principle (projection, email-keyed) | 1, 5 |
| `store.go` / `UserStore` | 1, 2 |
| `allowlist.go` / `Apply` replaces | 4 |
| `reconciler.go` / `project`, `Authenticate`, `Reconcile` | 5 |
| Removed: `instanceID`, `users`, `usersMu`, `unionStrings` | 6 (verified by grep in Step 3) |
| Data model, `Record`, `Store`, `UpdatedAt` | 1 |
| IP-to-CIDR conversion | already landed (`a256f2d`), reused in 3 and 6 |
| Concurrency: CAS, generation monotonicity | 1, 2 |
| Generation guard, strictly-older, equal-proceeds | 4 |
| Hot path / write-skip cache | 5 |
| Auth is synchronous | 5, 6 |
| Configuration `--reconcile-interval` | 6, 7 |
| Migration incl. conflicting-IP drop | 3 |
| Error handling, retries, reconcile logs-and-continues | 2, 4, 5 |
| Readiness stricter | 6 |
| RBAC | 7 |
| Testing (every listed assertion) | 1–5 |

**Two spec requirements needed explicit handling the spec did not fully specify**, and both are resolved in-plan rather than left ambiguous:

1. *Cache staleness across replicas.* The spec says the cache "is never a source of truth" but does not say how it stays honest when another replica changes a user's CIDR. Task 5 has `Reconcile` rebuild the cache from the loaded store, with `TestReconcileRefreshesCache` pinning it.
2. *Empty `sourceRange`.* A store with no users projects to an empty set. Whether the Traefik CRD accepts `sourceRange: []` is unverifiable from unit tests. Task 8 Step 1 checks it against a real API server with a documented fallback, rather than guessing.

**Placeholder scan.** No `TBD`, no "add error handling", no "similar to Task N". Every code step carries complete, compilable code.

**Type consistency.** Checked across tasks: `Store.upsert` (unexported, returns `bool`) vs `UserStore.Upsert` (exported, returns `(*Store, error)`) are deliberately distinct and used consistently — the fake in Task 5 calls the former inside the latter. `Allowlist.Apply(ctx, cidrs, generation)` has one signature everywhere. `Authenticate` returns `(bool, error)` in Task 5 and is consumed as `wrote, err` in Task 6. `equalStrings` and `newFakeClient` are defined once (Tasks 4 and 2 respectively) and reused across test files in the same package — Task 5's tests use `equalStrings` from Task 4, and Task 4's tests use `newFakeClient` from Task 2, which is valid because all files share `package main`.

**Ordering constraint.** Task 4's test file uses `newFakeClient`, defined in Task 2's test file. Task 5's test file uses `equalStrings`, defined in Task 4's test file. Tasks must be executed in order; they cannot be parallelised.
