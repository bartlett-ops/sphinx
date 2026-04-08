package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type user struct {
	Email string `json:"email"`
	IP    string `json:"ip"`
}

var (
	// TODO write users to middleware
	users         = make(map[string]user)
	dynClient     *dynamic.DynamicClient
	middlewareGVR = schema.GroupVersionResource{
		Group:    "traefik.io",
		Version:  "v1alpha1",
		Resource: "middlewares",
	}
	middlewareName      *string
	middlewareNamespace *string
)

func main() {
	port := flag.Int("port", 8080, "Port to run server on")
	trustedProxiesRaw := flag.String("trusted-proxies", "", "Comma separated list of trusted proxies in CIDR format")
	middlewareName = flag.String("middleware-name", "", "Name of allowlist middleware")
	middlewareNamespace = flag.String("middleware-namespace", "kube-system", "Namespace of middleware")
	flag.Parse()

	var trustedProxies []string

	if *trustedProxiesRaw != "" {
		trustedProxies = strings.Split(*trustedProxiesRaw, ",")
	}
	if *middlewareName == "" {
		log.Printf("Error: middleware-name not set")
		os.Exit(1)
	}

	config, err := clientcmd.BuildConfigFromFlags("", "/home/tom/.kube/config")
	if err != nil {
		log.Fatal(err)
	}

	// Create dynamic client
	dynClient, err = dynamic.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	middleware, err := getOrCreateMiddleware(middlewareName, middlewareNamespace)
	if err != nil {
		log.Fatal(err)
	}

	ips := middleware.Spec.IPAllowList.SourceRange
	log.Printf("Current allowlist: %v", ips)

	router := gin.Default()
	router.SetTrustedProxies(trustedProxies)
	router.GET("/users", getUsers)
	router.POST("/users", postUsers)

	router.Run(fmt.Sprintf(":%d", *port))
}

func addUser(u2 user) error {
	u1, exists := users[u2.Email]

	if !exists || u1 != u2 {
		users[u2.Email] = u2

		// Create set to ensure no duplicates
		set := make(map[string]struct{})
		for _, v := range users {
			set[v.IP] = struct{}{}
		}

		// Convert set to slice
		ips := make([]string, 0, len(set))
		for k := range set {
			ips = append(ips, k)
		}
		return updateMiddleware(middlewareName, middlewareNamespace, ips)
	}
	return nil
}

func getUnstructured(middleware *Middleware) (*unstructured.Unstructured, error) {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(middleware)
	if err != nil {
		log.Printf("conversion failed: %v", err)
	}
	return &unstructured.Unstructured{Object: obj}, err
}

func getUsers(c *gin.Context) {
	c.IndentedJSON(http.StatusOK, users)
}

func postUsers(c *gin.Context) {
	email := c.GetHeader("X-User-Email")
	if email == "" {
		c.JSON(400, gin.H{
			"error": "Missing X-User-Email header",
		})
	}
	user := user{
		Email: email,
		IP:    c.ClientIP(),
	}

	err := addUser(user)
	if err != nil {
		log.Println("Failed to add user")
		c.JSON(400, gin.H{
			"error": "Failed to add user",
		})
	} else {
		log.Println("Added user")
	}

	c.IndentedJSON(http.StatusCreated, user)
}
