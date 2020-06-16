package controller

import (
	"github.com/IBM/route-reflector-operator/pkg/controller/routereflector"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, routereflector.Add)
}
