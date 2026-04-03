package main

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type Middleware struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MiddlewareSpec `json:"spec"`
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: MiddlewareSpec{},
	}
}
