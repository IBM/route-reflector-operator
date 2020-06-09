
include Makefile.env

.PHONY: lint
lint:
ifdef GOLANGCI_LINT_EXISTS
	golangci-lint run
else
	@echo "golangci-lint is not installed"
endif

.PHONY: lint-fmt
lint-fmt:
	@if [ -n "$$(gofmt -l ${GOFILES})" ]; then echo 'Please run `make fmt` on your code.' && exit 1; fi

.PHONY: fmt
fmt:
ifdef GOLANGCI_LINT_EXISTS
	golangci-lint run --disable-all --enable=gofmt --fix
else
	@echo "golangci-lint is not installed"
endif

update-operator-resource:
	operator-sdk generate crds
	operator-sdk generate k8s

build-operator:
ifndef IMG
	$(error Missing image, please define IMG)
endif
	operator-sdk build $(IMG)

publish-image: build-operator
	docker push $(IMG)

deploy-operator:
	cat deploy/operator.yaml | sed 's|REPLACE_IMAGE|$(IMG)|g' > deploy/operator.dev.yaml
	-kubectl apply -f deploy/operator.dev.yaml

apply-resources:
	-kubectl apply -f deploy/service_account.yaml
	-kubectl apply -f deploy/role.yaml
	-kubectl apply -f deploy/role_binding.yaml
	-kubectl apply -f deploy/crds/route-reflector.ibm.com_routereflectors_crd.yaml
	-kubectl apply -f deploy/crds/route-reflector.ibm.com_v1_routereflector_cr.yaml

copy-secret:
	$(eval export SECRETNAME=$(shell sh -c "if kubectl get secret default-us-icr-io &>/dev/null; then echo 'default-us-icr-io'; else echo 'all-icr-io'; fi"))
	-kubectl get secret $(SECRETNAME) -n default -o yaml | sed -e 's/namespace: default/namespace: kube-system/' -e 's/$(SECRETNAME)/default-us-icr-io/' | kubectl create -f - &>/dev/null

logsf:
	kubectl -nkube-system logs -f $$(kubectl -nkube-system -lname=route-reflector-operator get pods -o=name)

slogsf:
	sleep 15
	kubectl -nkube-system logs -f $$(kubectl -nkube-system -lname=route-reflector-operator get pods -o=name)

watch:
	watch -n1 "kubectl get nodes -l route-reflector=true && echo "==========" && calicoctl get bgpconfig -oyaml --config=$C && echo "==========" && calicoctl get bgppeers --config=$C"

restart:
	kubectl -nkube-system delete $$(kubectl -nkube-system -lname=route-reflector-operator get pods -o=name)

all: update-operator-resource build-operator publish-image copy-secret apply-resources deploy-operator slogsf

cleanup:
	kubectl delete -f deploy/service_account.yaml
	kubectl delete -f deploy/role.yaml
	kubectl delete -f deploy/role_binding.yaml
	kubectl delete -f deploy/operator.dev.yaml
	kubectl delete -f deploy/crds/route-reflector.ibm.com_routereflectors_crd.yaml
	rm -f deploy/operator.dev.yaml

.PHONY: updatedeps
updatedeps:
	go get -u=patch ./...
	go mod tidy

#
# Travis CI/CD
#

deps:
	make _deps-$(shell uname | tr '[:upper:]' '[:lower:]')

_deps-darwin:
	$(error Operating system not supported)

_deps-linux:
	curl -sL https://github.com/operator-framework/operator-sdk/releases/download/v${OP_SDK_RELEASE_VERSION}/operator-sdk-v${OP_SDK_RELEASE_VERSION}-x86_64-linux-gnu > ${INSTALL_LOCATION}/operator-sdk
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b ${INSTALL_LOCATION} v${GOLANGCI_LINT_VERSION}
	curl -sL https://github.com/kubernetes-sigs/kind/releases/download/v${KIND_VERSION}/kind-linux-amd64 > ${INSTALL_LOCATION}/kind
	curl -sL https://storage.googleapis.com/kubernetes-release/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl > ${INSTALL_LOCATION}/kubectl
	chmod +x ${INSTALL_LOCATION}/operator-sdk ${INSTALL_LOCATION}/kind ${INSTALL_LOCATION}/kubectl

_calculate-build-number:
    $(eval export CONTAINER_VERSION?=$(GIT_COMMIT_SHA)-$(shell date "+%s"))

lint:
ifdef GOLANGCI_LINT_EXISTS
	golangci-lint run --verbose --timeout 3m
else
	@echo "golangci-lint is not installed"
endif

lint-sh:
ifdef SHELLCHECK_EXISTS
	shellcheck ${SHFILES}
else
	@echo "shellcheck is not installed"
endif

formatcheck:
	([ -z "$(shell gofmt -d $(GO_FILES))" ]) || (echo "Source is unformatted, please execute make format"; exit 1)

format:
	@gofmt -w ${GO_FILES}

vet:
	go vet ${GO_PACKAGES}

test:
	go test -race -timeout 60s -covermode=atomic -coverprofile=cover.out ${GO_PACKAGES}

validate-code: lint lint-sh formatcheck vet test

build-operator: validate-code
	operator-sdk build quay.io/example/route-reflector-operator

fvt: _calculate-build-number build-operator
	docker tag $(REGISTRY_REPO) $(REGISTRY_REPO):$(CONTAINER_VERSION)
	$(eval export REGISTRY_REPO?=$(REGISTRY_REPO))
	@scripts/run-fvt.sh
