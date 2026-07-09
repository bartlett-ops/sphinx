package main

import (
	"context"
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
	router.GET("/ready", readiness(reconciler, *reconcileInterval))
	router.GET("/users", getUsers(store))
	router.POST("/users", auth(reconciler)) // Backwards compatibility
	router.GET("/auth", auth(reconciler))   // Backwards compatibility

	if err := router.Run(fmt.Sprintf(":%d", *port)); err != nil {
		log.Fatal(err)
	}
}

// readiness reports ready only once a reconcile has succeeded recently. This
// subsumes probing the middleware and ConfigMap directly: Reconcile reads one
// and writes the other, so an unreachable resource already fails it.
func readiness(r *Reconciler, interval time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !r.Healthy(time.Now(), 3*interval) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no successful reconcile within 3 intervals"})
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
