.PHONY: dev

dev:
	go run . --middleware-name sphinx-dev --configmap-name sphinx-dev-users --middleware-namespace kube-system --trusted-proxies 127.0.0.1/32 --reconcile-interval 30s

build:
	CGO_ENABLED=0 GOOS=linux go build -o sphinx .
