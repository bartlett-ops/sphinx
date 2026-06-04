package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	u, err := dynClient.Resource(middlewareGVR).Namespace(*namespace).Get(context.TODO(), *name, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, err
		}
		m := NewMiddleware(*name, *namespace)
		u, err = createMiddleware(m)
		if err != nil {
			log.Printf("Failed to create new middleware: %v", err)
			return nil, err
		}
		log.Printf("Created new middleware: %v", m)
	}
	var middleware Middleware
	if err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &middleware); err != nil {
		log.Printf("Conversion failed when reading middleware: %v", err)
		return nil, err
	}
	return &middleware, nil
}

func createMiddleware(middleware *Middleware) (*unstructured.Unstructured, error) {
	u, err := getUnstructured(middleware)
	if err != nil {
		return nil, err
	}
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

func loadUsers() error {
	u, err := dynClient.Resource(configMapGVR).Namespace(*middlewareNamespace).Get(context.TODO(), *configMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	data, found, err := unstructured.NestedString(u.Object, "data", "users")
	if err != nil || !found || data == "" {
		return err
	}
	return json.Unmarshal([]byte(data), &users)
}

func saveUsers() error {
	usersMu.RLock()
	data, err := json.Marshal(users)
	usersMu.RUnlock()
	if err != nil {
		return err
	}

	existing, err := dynClient.Resource(configMapGVR).Namespace(*middlewareNamespace).Get(context.TODO(), *configMapName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		cm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      *configMapName,
					"namespace": *middlewareNamespace,
				},
				"data": map[string]interface{}{
					"users": string(data),
				},
			},
		}
		_, err = dynClient.Resource(configMapGVR).Namespace(*middlewareNamespace).Create(context.TODO(), cm, metav1.CreateOptions{})
		return err
	}

	if err = unstructured.SetNestedField(existing.Object, string(data), "data", "users"); err != nil {
		return err
	}
	_, err = dynClient.Resource(configMapGVR).Namespace(*middlewareNamespace).Update(context.TODO(), existing, metav1.UpdateOptions{})
	return err
}

func updateMiddleware(name *string, namespace *string, ips []string) error {
	const maxRetries = 5
	for range maxRetries {
		u, err := dynClient.Resource(middlewareGVR).Namespace(*namespace).Get(context.TODO(), *name, metav1.GetOptions{})
		if err != nil {
			log.Printf("Failed to get middleware: %v", err)
			return err
		}
		if err = mutate(u, ips); err != nil {
			log.Printf("Failed to mutate middleware: %v", err)
			return err
		}
		_, err = dynClient.Resource(middlewareGVR).Namespace(*namespace).Update(context.TODO(), u, metav1.UpdateOptions{})
		if err == nil {
			log.Printf("Updated middleware")
			return nil
		}
		if !errors.IsConflict(err) {
			log.Printf("Failed to update middleware: %v", err)
			return err
		}
		log.Printf("Resource conflict, retrying: %v", err)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("update middleware: exceeded %d retries", maxRetries)
}
