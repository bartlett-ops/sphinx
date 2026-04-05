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
	middleware *Middleware
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

	middleware, err = getOrCreateMiddleware(middlewareName, middlewareNamespace)
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

func getOrCreateMiddleware(name *string, namespace *string) (*Middleware, error) {
	var middleware *Middleware
	var err error
	u, err := dynClient.Resource(middlewareGVR).Namespace(*namespace).Get(context.TODO(), *name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			middleware = NewMiddleware(*name, *namespace)
			// write empty middleware
			err = writeMiddleware(middleware)
			if err != nil {
				log.Printf("Failed to create new middleware: %v", err)
			} else {
				log.Printf("Created new middleware: %v", middleware)
			}
		}
	} else {
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &middleware)
		if err != nil {
			log.Printf("Conversion failed when reading middleware: %v", err)
		}
	}
	return middleware, err
}

func addUser(middleware *Middleware, u2 user) error {
	u1, exists := users[u2.Email]

	if !exists || u1 != u2 {
		users[u2.Email] = u2
		return writeMiddleware(middleware)
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

func writeMiddleware(middleware *Middleware) error {
	_, err := getMiddleware(middleware)
	if err != nil {
		if errors.IsNotFound(err) {
			err = createMiddleware(middleware)
		} else {
			log.Printf("Failed to check for existing middleware: %v", err)
		}
	} else {
		err = updateMiddleware(middleware)
	}
	return err
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

	err := addUser(middleware, user)
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
