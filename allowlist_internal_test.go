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
func TestAllowlistCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("absent middleware is a failure", func(t *testing.T) {
		c := newFakeClient(t)
		if err := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist").Check(ctx); err == nil {
			t.Error("Check on absent middleware = nil, want error; EnsureExists created it at startup")
		}
	})

	t.Run("present middleware is reachable", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, []string{"203.0.113.7/32"}, nil))
		if err := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist").Check(ctx); err != nil {
			t.Errorf("Check = %v, want nil", err)
		}
	})
}

func TestAllowlistApplyReplacesRatherThanUnions(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t, middlewareWith(t, []string{"203.0.113.7/32"}, nil))
	a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

	if err := a.Apply(ctx, []string{"198.51.100.4/32"}, 1, "uid-1"); err != nil {
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
			map[string]string{generationAnnotation: "43", storeUIDAnnotation: "uid-1"}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 42, "uid-1"); err != nil {
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
			map[string]string{generationAnnotation: "42", storeUIDAnnotation: "uid-1"}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 42, "uid-1"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want drift repaired to [203.0.113.7/32]", got)
		}
	})

	t.Run("missing annotation proceeds", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, []string{"1.2.3.4/32"}, nil))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 7, "uid-1"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want [203.0.113.7/32]", got)
		}
	})

	t.Run("malformed annotation is treated as zero", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t,
			[]string{"1.2.3.4/32"},
			map[string]string{generationAnnotation: "banana", storeUIDAnnotation: "uid-1"}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 1, "uid-1"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want the write to proceed", got)
		}
	})

	t.Run("stamps the generation it applied", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, nil, nil))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 42, "uid-1"); err != nil {
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

func TestAllowlistGenerationGuardAcrossStoreLineages(t *testing.T) {
	ctx := context.Background()

	t.Run("older generation from the same store is skipped", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, []string{"198.51.100.4/32"}, map[string]string{
			generationAnnotation: "43",
			storeUIDAnnotation:   "uid-1",
		}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 42, "uid-1"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"198.51.100.4/32"}) {
			t.Errorf("sourceRange = %v, want the newer generation to survive", got)
		}
	})

	// A recreated ConfigMap restarts the counter. Skipping here would freeze the
	// allowlist forever, stale CIDRs and all.
	t.Run("older generation from a different store proceeds", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, []string{"198.51.100.4/32"}, map[string]string{
			generationAnnotation: "43",
			storeUIDAnnotation:   "uid-1",
		}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 1, "uid-2"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want the recreated store's projection to win", got)
		}
	})

	// A middleware from before this annotation existed carries no uid.
	t.Run("older generation with no stamped uid proceeds", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, []string{"198.51.100.4/32"}, map[string]string{
			generationAnnotation: "43",
		}))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 1, "uid-1"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readSourceRange(t, c); !equalStrings(got, []string{"203.0.113.7/32"}) {
			t.Errorf("sourceRange = %v, want the write to proceed", got)
		}
	})

	t.Run("stamps the store uid it applied", func(t *testing.T) {
		c := newFakeClient(t, middlewareWith(t, nil, nil))
		a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

		if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 7, "uid-9"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		u, err := c.Resource(middlewareGVR).Namespace("kube-system").
			Get(ctx, "sphinx-allowlist", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get middleware: %v", err)
		}
		if got := stampedStoreUID(u); got != "uid-9" {
			t.Errorf("stamped store uid = %q, want %q", got, "uid-9")
		}
	})
}

func TestAllowlistApplyEmptySet(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t, middlewareWith(t, []string{"203.0.113.7/32"}, nil))
	a := newTraefikAllowlist(c, "kube-system", "sphinx-allowlist")

	if err := a.Apply(ctx, nil, 1, "uid-1"); err != nil {
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
	if err := a.Apply(ctx, []string{"203.0.113.7/32"}, 1, "uid-1"); err != nil {
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
