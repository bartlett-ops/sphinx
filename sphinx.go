// Package sphinx a demo plugin.
package sphinx

import (
	"context"
	"fmt"
	"net/http"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/yaml"
)

// Config the plugin configuration.
type Config struct {
	Headers map[string]string `json:"headers,omitempty"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		Headers: make(map[string]string),
	}
}

// Demo a Demo plugin.
type Demo struct {
	next      http.Handler
	headers   map[string]string
	name      string
	template  *template.Template
	clientset *kubernetes.Clientset
	ctx       context.Context
}

// New created a new Demo plugin.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if len(config.Headers) == 0 {
		return nil, fmt.Errorf("headers cannot be empty")
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		panic(err.Error())
	}

	return &Demo{
		headers:   config.Headers,
		next:      next,
		name:      name,
		template:  template.New("sphinx").Delims("[[", "]]"),
		clientset: clientset,
		ctx:       ctx,
	}, nil
}

func (a *Demo) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	clientIP := req.Header.Get("X-Fowarded-For")
	if clientIP == "" {
		http.Error(rw, "Failed to parse X-Fowarded-For", http.StatusInternalServerError)
		return
	}

	userEmail := req.Header.Get("X-User-Email")
	if userEmail == "" {
		http.Error(rw, "Failed to parse X-User-Email", http.StatusInternalServerError)
	}

	configMap, err := a.clientset.CoreV1().ConfigMaps("kube-system").Get(a.ctx, "sphinx", metav1.GetOptions{})
	if err != nil {
		configMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sphinx",
				Namespace: "kube-system",
			},
			Data: map[string]string{
				"userList": "",
			},
		}
	}

	var userList map[string]any
	err = yaml.Unmarshal([]byte(configMap.Data["userList"]), &userList)
	if err != nil {
		panic(err)
	}
	userList[userEmail] = clientIP
	a.clientset.CoreV1().ConfigMaps("kube-system").Update(a.ctx, configMap, metav1.UpdateOptions{})

	a.next.ServeHTTP(rw, req)
}
