package main

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
		time.Sleep(backoff(attempt))
	}
	return nil, fmt.Errorf("upsert %s: exceeded %d retries: %w", email, maxRetries, last)
}
