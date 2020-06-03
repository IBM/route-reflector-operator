package controller

import (
	"github.ibm.com/alchemy-containers/armada-route-reflector-operator/pkg/controller/node"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, node.Add)
}
