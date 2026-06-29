package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/peterbourgon/ff/v3"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type user struct {
	Email string `json:"email"`
	IP    string `json:"ip"`
}

var (
	usersMu             sync.RWMutex
	users               = make(map[string]user)
	dynClient           *dynamic.DynamicClient
	middlewareGVR       = schema.GroupVersionResource{
		Group:    "traefik.io",
		Version:  "v1alpha1",
		Resource: "middlewares",
	}
	configMapGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "configmaps",
	}
	middlewareName      *string
	middlewareNamespace *string
	configMapName       *string
	instanceID          string
)

func main() {
	port := flag.Int("port", 8080, "Port to run server on")
	trustedProxiesRaw := flag.String("trusted-proxies", "", "Comma separated list of trusted proxies in CIDR format")
	middlewareName = flag.String("middleware-name", "", "Name of allowlist middleware")
	middlewareNamespace = flag.String("middleware-namespace", "kube-system", "Namespace of middleware")
	configMapName = flag.String("configmap-name", "sphinx-users", "Name of ConfigMap for user persistence")
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig file (auto-detected if not set)")
	if err := ff.Parse(flag.CommandLine, os.Args[1:], ff.WithEnvVarPrefix("SPHINX")); err != nil {
		log.Fatal(err)
	}

	var trustedProxies []string

	if *trustedProxiesRaw != "" {
		trustedProxies = strings.Split(*trustedProxiesRaw, ",")
	}
	if *middlewareName == "" {
		log.Printf("Error: middleware-name not set")
		os.Exit(1)
	}

	hostname, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	instanceID = hostname

	config, err := resolveKubeConfig(*kubeconfig)
	if err != nil {
		log.Fatal(err)
	}

	// Create dynamic client
	dynClient, err = dynamic.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	if _, err = getOrCreateMiddleware(middlewareName, middlewareNamespace); err != nil {
		log.Fatal(err)
	}

	if err = loadUsers(); err != nil {
		log.Fatal(err)
	}
	log.Printf("Loaded %d users", len(users))

	ips := getIPsFromUsers()
	if err = updateMiddleware(middlewareName, middlewareNamespace, ips); err != nil {
		log.Fatalf("Failed to sync middleware on startup: %v", err)
	}
	log.Printf("Current allowlist: %v", ips)

	router := gin.Default()
	router.SetTrustedProxies(trustedProxies)
	router.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.GET("/ready", readiness)
	router.GET("/users", getUsers)
	router.POST("/users", postUsers)

	router.Run(fmt.Sprintf(":%d", *port))
}

func readiness(c *gin.Context) {
	ctx := c.Request.Context()
	if _, err := dynClient.Resource(middlewareGVR).Namespace(*middlewareNamespace).Get(ctx, *middlewareName, metav1.GetOptions{}); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("middleware unavailable: %v", err)})
		return
	}
	_, err := dynClient.Resource(configMapGVR).Namespace(*middlewareNamespace).Get(ctx, *configMapName, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("configmap unavailable: %v", err)})
		return
	}
	c.Status(http.StatusOK)
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

func addUser(u2 user) error {
	usersMu.Lock()
	u1, exists := users[u2.Email]
	if exists && u1 == u2 {
		usersMu.Unlock()
		return nil
	}
	users[u2.Email] = u2
	ips := getIPsFromUsers()
	usersMu.Unlock()

	if err := saveUsers(); err != nil {
		return err
	}
	return updateMiddleware(middlewareName, middlewareNamespace, ips)
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

func getIPsFromUsers() []string {
	set := make(map[string]struct{})
	for _, v := range users {
		set[v.IP] = struct{}{}
	}
	ips := make([]string, 0, len(set))
	for k := range set {
		ips = append(ips, k)
	}
	return ips
}

func getUnstructured(middleware *Middleware) (*unstructured.Unstructured, error) {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(middleware)
	if err != nil {
		log.Printf("conversion failed: %v", err)
	}
	return &unstructured.Unstructured{Object: obj}, err
}

func getUsers(c *gin.Context) {
	usersMu.RLock()
	defer usersMu.RUnlock()
	c.IndentedJSON(http.StatusOK, users)
}

func postUsers(c *gin.Context) {
	email := c.GetHeader("X-User-Email")
	if email == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Missing X-User-Email header",
		})
		return
	}
	user := user{
		Email: email,
		IP:    resolveClientIP(c),
	}

	if err := addUser(user); err != nil {
		log.Println("Failed to add user")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to add user",
		})
		return
	}
	log.Println("Added user")
	c.IndentedJSON(http.StatusCreated, user)
}
