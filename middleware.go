package main

import (
	"context"
	"log"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

type Middleware struct {
	metav1.TypeMeta `json:",inline"`

	Metadata Metadata       `json:"metadata"`
	Spec     MiddlewareSpec `json:"spec"`
}

type Metadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	ResourceVersion string            `json:"resourceVersion"`
}

type MiddlewareSpec struct {
	IPAllowList IPAllowList `json:"ipAllowList"`
}

type IPAllowList struct {
	IPStrategy       IPStrategy `json:"ipStrategy"`
	RejectStatusCode int        `json:"rejectStatusCode"`
	SourceRange      []string   `json:"sourceRange"`
}

type IPStrategy struct {
	Depth       int      `json:"depth"`
	ExcludedIPs []string `json:"excludedIPs"`
	IPv6Subnet  int      `json:"ipv6Subnet"`
}

func NewMiddleware(name, namespace string) *Middleware {
	return &Middleware{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "traefik.io/v1alpha1",
			Kind:       "Middleware",
		},
		Metadata: Metadata{
			Name:      name,
			Namespace: namespace,
		},
		Spec: MiddlewareSpec{},
	}
}

func getOrCreateMiddleware(name *string, namespace *string) (*Middleware, error) {
	var middleware *Middleware
	var err error
	u, err := dynClient.Resource(middlewareGVR).Namespace(*namespace).Get(context.TODO(), *name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			middleware = NewMiddleware(*name, *namespace)
			// write empty middleware
			u, err = createMiddleware(middleware)
			if err != nil {
				log.Printf("Failed to create new middleware: %v", err)
				return nil, err
			} else {
				log.Printf("Created new middleware: %v", middleware)
			}
		}
	}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &middleware)
	if err != nil {
		log.Printf("Conversion failed when reading middleware: %v", err)
	}
	return middleware, err
}

func createMiddleware(middleware *Middleware) (*unstructured.Unstructured, error) {
	u, _ := getUnstructured(middleware)
	unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
	u2, err := dynClient.Resource(middlewareGVR).Namespace(middleware.Metadata.Namespace).Create(context.TODO(), u, metav1.CreateOptions{})
	if err != nil {
		log.Printf("Failed to create new middleware: %v", err)
	}
	return u2, err
}

func mutate(middleware *unstructured.Unstructured, ips []string) error {
	return unstructured.SetNestedStringSlice(middleware.Object, ips, "spec", "ipAllowList", "sourceRange")
}

func updateMiddleware(name *string, namespace *string, ips []string) error {
	middleware, err := getOrCreateMiddleware(name, namespace)
	if err != nil {
		log.Printf("Failed to obtain middleware: %v", err)
		return err
	}
	u, _ := getUnstructured(middleware)

	err = mutate(u, ips)
	if err != nil {
		log.Printf("Failed to mutate middleware: %v", err)
		return err
	}
	const maxRetries = 5
	for range maxRetries {
		_, err = dynClient.Resource(middlewareGVR).Namespace(middleware.Metadata.Namespace).Update(context.TODO(), u, metav1.UpdateOptions{})
		if err != nil {
			if errors.IsConflict(err) {
				log.Printf(err.Error())
				log.Printf("Resource conflict, retrying")
				time.Sleep(2 * time.Second)
				continue
			} else {
				log.Printf("Failed to update middleware: %v", err)
				break
			}
		} else {
			log.Printf("Updated middleware")
			break
		}
	}
	return err
}

//func writeMiddleware(middleware *Middleware) error {
//	_, err := getMiddleware(middleware)
//	if err != nil {
//		if errors.IsNotFound(err) {
//			err = createMiddleware(middleware)
//		} else {
//			log.Printf("Failed to check for existing middleware: %v", err)
//		}
//	} else {
//		err = updateMiddleware(middleware)
//	}
//	return err
//}
