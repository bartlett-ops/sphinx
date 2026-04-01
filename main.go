package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type user struct {
	Email string `json:"email"`
	IP    string `json:"ip"`
}

var (
	users         = make(map[string]user)
	dynClient     *dynamic.DynamicClient
	middlewareGVR = schema.GroupVersionResource{
		Group:    "traefik.containo.us",
		Version:  "v1alpha1",
		Resource: "middlewares",
	}
	middleware *unstructured.Unstructured
)

func main() {
	port := flag.Int("port", 8080, "Port to run server on")
	trustedProxiesRaw := flag.String("trusted-proxies", "", "Comma separated list of trusted proxies in CIDR format")
	middlewareName := flag.String("middleware-name", "", "Name of allowlist middleware")
	middlewareNamespace := flag.String("middleware-namespace", "kube-system", "Namespace of middleware")
	flag.Parse()

	var trustedProxies []string

	if *trustedProxiesRaw != "" {
		trustedProxies = strings.Split(*trustedProxiesRaw, ",")
	}
	if *middlewareName == "" {
		fmt.Println("Error: middleware-name not set")
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

	middleware, err = getOrCreateMiddleware(middlewareName, middlewareNamespace)
	if err != nil {
		log.Fatal(err)
	}

	ips, _, _ := unstructured.NestedStringSlice(middleware.Object, "spec", "ipAllowList", "sourceRange")
	fmt.Println("Current allowlist:", ips)

	router := gin.Default()
	router.SetTrustedProxies(trustedProxies)
	router.GET("/users", getUsers)
	router.POST("/users", postUsers)

	router.Run(fmt.Sprintf(":%d", *port))
}

func getOrCreateMiddleware(name *string, namespace *string) (*unstructured.Unstructured, error) {
	var middleware *unstructured.Unstructured
	var err error
	middleware, err = dynClient.Resource(middlewareGVR).Namespace(*namespace).Get(context.TODO(), *name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			middleware = &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": fmt.Sprintf("%s/%s", middlewareGVR.Group, middlewareGVR.Version),
					"kind":       "Middleware",
					"metadata": map[string]any{
						"name":      name,
						"namespace": namespace,
					},
					"spec": map[string]any{
						// Example spec: add your configuration here
						"ipAllowList": map[string]any{
							"sourceRange": []string{},
						},
					},
				},
			}
		}
		err = nil
	}
	return middleware, err
}

func addUser(u2 user) {
	u1, exists := users[u2.Email]

	if !exists || u1 != u2 {
		users[u2.Email] = u2
		// sync
	}
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

	addUser(user)

	c.IndentedJSON(http.StatusCreated, user)
}
