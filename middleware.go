package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Middleware struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        Metadata `json:"metadata"`

	Spec MiddlewareSpec `json:"spec"`
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

func getMiddleware(middleware *Middleware) (*unstructured.Unstructured, error) {
	u, err := dynClient.Resource(middlewareGVR).Namespace(middleware.Metadata.Namespace).Get(context.TODO(), middleware.Metadata.Name, metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to get middleware: %v", err)
	}
	return u, err
}

func createMiddleware(middleware *Middleware) error {
	u, _ := getUnstructured(middleware)
	unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
	_, err := dynClient.Resource(middlewareGVR).Namespace(middleware.Metadata.Namespace).Create(context.TODO(), u, metav1.CreateOptions{})
	if err != nil {
		log.Printf("Failed to create new middleware: %v", err)
	}
	return err
}

func mergeUnstructured(u1, u2 *unstructured.Unstructured) error {
	j1, _ := json.Marshal(u1.Object)
	j2, _ := json.Marshal(u2.Object)
	patch, _ := jsonpatch.CreateMergePatch(j1, j2)
	mergedJSON, _ := jsonpatch.MergePatch(j1, patch)

	var merged map[string]any
	err := json.Unmarshal(mergedJSON, &merged)
	if err != nil {
		log.Printf("Failed to merge objects: %v", err)
		return err
	}

	u1.Object = merged
	return nil
}

func updateMiddleware(middleware *Middleware) error {
	u, _ := getUnstructured(middleware)
	liveMiddleware, err := getMiddleware(middleware)
	if err != nil {
		log.Printf("Failed to get live middleware")
		return err
	}
	err = mergeUnstructured(u, liveMiddleware)
	if err != nil {
		return err
	}

	_, err = dynClient.Resource(middlewareGVR).Namespace(middleware.Metadata.Namespace).Update(context.TODO(), u, metav1.UpdateOptions{})
	if err != nil {
		if errors.IsConflict(err) {
			log.Printf("Resource conflict, retrying")
			time.Sleep(2 * time.Second)
			err = updateMiddleware(middleware)
		} else {
			log.Printf("Failed to update middleware: %v", err)
		}
	} else {
		log.Printf("Updated middleware")
	}
	return err
}
