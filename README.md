
Documenatation at [doc/rr-operator-doc.md](doc/rr-operator-doc.md).

#### Deploy

```
IMG="us.icr.io/smz2/rr-operator:l" make all
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