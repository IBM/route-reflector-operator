
Documenatation at [doc/rr-operator-doc.md](doc/rr-operator-doc.md).

#### Deploy

```
IMG="us.icr.io/smz2/route-reflector-operator:l" make all
```

#### Cleanup

```
make cleanup
```

The Node and Calico objects:
```
for n in $(kgno -l route-reflector=true -o=name --no-headers); do
  c get node $(k get ${n} -oyaml|grep worker-id|awk '{print $2}') -oyaml --config=$C | sed '/routeReflectorClusterID/d' | c apply -f - --config=$C
  k label ${n} route-reflector-
done

calicoctl delete bgpconfig default --config=${C}
calicoctl delete bgppeers peer-with-route-reflectors --config=${C}
```

#### Running `make fmt`

There is a prerequisite for running `make fmt` that you have `golangci-lint` installed.

Please install the latest version by running:
```bash
curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | sh -s -- -b $(go env GOPATH)/bin v1.29.0
```
