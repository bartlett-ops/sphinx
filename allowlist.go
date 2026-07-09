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

const (
	// generationAnnotation records the store generation last written.
	generationAnnotation = "sphinx.bartlett.ops/generation"
	// storeUIDAnnotation records which store lineage that generation belongs to.
	storeUIDAnnotation = "sphinx.bartlett.ops/store-uid"
)

// Allowlist writes the desired CIDR set. Implementations know nothing about
// users; the set is already projected.
type Allowlist interface {
	Apply(ctx context.Context, cidrs []string, generation int64, storeUID string) error
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

// stampedStoreUID reads which store lineage the stamped generation belongs to.
// An absent annotation reads as "", which never equals a real uid.
func stampedStoreUID(u *unstructured.Unstructured) string {
	raw, found, err := unstructured.NestedString(u.Object, "metadata", "annotations", storeUIDAnnotation)
	if err != nil || !found {
		return ""
	}
	return raw
}

// Apply replaces spec.ipAllowList.sourceRange with exactly cidrs. It never
// unions: that is what let a re-authenticating user's old CIDR survive.
//
// A write whose generation is strictly older than the stamped one is skipped,
// so a late replica cannot resurrect a removed CIDR. An equal generation
// proceeds, which is how the background reconcile repairs drift.
//
// The generation counter only orders writes within one store lineage: it is
// scoped by storeUID, so a ConfigMap that was deleted and recreated (and thus
// restarted its counter) is recognized as a fresh start rather than skipped as
// stale.
func (a *traefikAllowlist) Apply(ctx context.Context, cidrs []string, generation int64, storeUID string) error {
	if cidrs == nil {
		cidrs = []string{}
	}
	var last error
	for attempt := range maxRetries {
		u, err := a.resource().Get(ctx, a.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get middleware: %w", err)
		}

		// The generation counter only orders writes from one store. A recreated
		// ConfigMap restarts it, so a lower generation from a different lineage
		// is a fresh start, not a stale write, and must not be skipped.
		stamped := stampedGeneration(u)
		if stampedStoreUID(u) == storeUID && generation < stamped {
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
		if err := unstructured.SetNestedField(u.Object, storeUID, "metadata", "annotations", storeUIDAnnotation); err != nil {
			return fmt.Errorf("set store-uid annotation: %w", err)
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
