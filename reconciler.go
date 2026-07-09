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
