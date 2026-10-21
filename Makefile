# Images go to the node-local registry on server1 (kharness/registry, 127.0.0.1:5000 on the node) through an ssh
# tunnel; the node pulls them as localhost:5000/kharness:<tag>.
KUBECONFIG ?= $(HOME)/.kube/server1.yaml
SSH_HOST   ?= server1
TAG        ?= $(shell date +%Y%m%d-%H%M%S)
TAG        := $(TAG)
PUSH_REPO  := 127.0.0.1:5000/kharness
NODE_REPO  := localhost:5000/kharness
SOCK       := /tmp/kharness-push-$(USER).sock
CRANE      ?= go run github.com/google/go-containerregistry/cmd/crane@v0.22.1

.PHONY: build test vet image push deploy deploy-dry tui clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/kharness ./cmd/kharness

test:
	go test ./...

vet:
	go vet ./...

image: build
	docker build -t $(PUSH_REPO):$(TAG) -t $(PUSH_REPO):latest .

# Docker Desktop's daemon runs in a VM and can't reach the tunnel on the laptop's loopback, so crane pushes from here.
push: image
	docker save $(PUSH_REPO):$(TAG) -o /tmp/kharness-image-$(USER).tar
	ssh -fNM -S $(SOCK) -o ExitOnForwardFailure=yes -L 127.0.0.1:5000:127.0.0.1:5000 $(SSH_HOST)
	$(CRANE) push --insecure /tmp/kharness-image-$(USER).tar $(PUSH_REPO):$(TAG) && $(CRANE) tag --insecure $(PUSH_REPO):$(TAG) latest; \
	  rc=$$?; ssh -S $(SOCK) -O exit $(SSH_HOST) 2>/dev/null; exit $$rc
	@echo "pushed $(NODE_REPO):$(TAG)"

# Needs OPENROUTER_API_KEY (first install) and optionally TELEGRAM_BOT_TOKEN in the environment.
deploy: push
	./bin/kharness --kubeconfig $(KUBECONFIG) bootstrap --image $(NODE_REPO):$(TAG) $(BOOTSTRAP_FLAGS)

deploy-dry: build
	./bin/kharness --kubeconfig $(KUBECONFIG) bootstrap --dry-run $(BOOTSTRAP_FLAGS)

tui: build
	./bin/kharness tui

clean:
	rm -rf bin
