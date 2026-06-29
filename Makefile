.PHONY: dev

dev:
	go run . --middleware-name sphinx-allowlist --middleware-namespace kube-system --trusted-proxies 0.0.0.0/0
