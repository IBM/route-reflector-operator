GO111MODULE := on
GOLANGCI_LINT_EXISTS:=$(shell golangci-lint --version 2>/dev/null)

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

apply-role:
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

all: update-operator-resource build-operator publish-image copy-secret apply-role deploy-operator slogsf

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
