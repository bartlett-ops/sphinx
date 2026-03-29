.PHONY: lint test vendor clean

export GO111MODULE=on

default: lint test

lint:
	golangci-lint run

test:
	go test -v -cover ./...

yaegi_test:
	yaegi test -v .

vendor:
	go mod vendor

clean:
	rm -rf ./vendor

deploy:
	kubectl create configmap plugin-sphinx -n kube-system --from-file=./ -o yaml --dry-run=client | kubectl apply -f -
	kubectl rollout restart -n kube-system deployment/traefik

