GO111MODULE := on

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

copy-secret:
	-kubectl get secret default-us-icr-io -n default -o yaml | sed -e 's/namespace: default/namespace: kube-system/' -e 's/default-us-icr-io/default-us-icr-io/' | kubectl create -f -

logsf:
	kubectl -nkube-system logs -f $$(kubectl -nkube-system -lname=route-reflector-operator get pods -o=name)

slogsf:
	sleep 5
	kubectl -nkube-system logs -f $$(kubectl -nkube-system -lname=route-reflector-operator get pods -o=name)

watch:
	watch -n1 "kubectl get nodes -l route-reflector=true && echo "==========" && calicoctl get bgpconfig -oyaml --config=$C && echo "==========" && calicoctl get bgppeers -oyaml --config=$C"

all: update-operator-resource build-operator publish-image copy-secret apply-role deploy-operator slogsf

cleanup:
	kubectl delete -f deploy/service_account.yaml
	kubectl delete -f deploy/role.yaml
	kubectl delete -f deploy/role_binding.yaml
	kubectl delete -f deploy/operator.dev.yaml
	rm -fi deploy/operator.dev.yaml

.PHONY: updatedeps
updatedeps:
	go get -u=patch ./...
	go mod tidy
