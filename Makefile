IMAGE ?= nekromancer:dev
ENVTEST_K8S ?= 1.37.0
ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23

.PHONY: build test lint image
build:
	go build -o bin/ ./cmd/...

test:
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S) -p path)" go test ./...
	hack/package-chart_test.sh
	hack/chart_test.sh

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"
	helm lint charts/nekromancer

image:
	docker build -t $(IMAGE) .

.PHONY: e2e
e2e:
	hack/e2e.sh
