package kairostest

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// quick and dirty implementation of the runtime.Object interface that does not
// include the schema.ObjectKind interface (to cause a panic on the type assertion)
type GetObjectMetaAndKindPanicType struct{}

func (p GetObjectMetaAndKindPanicType) GetObjectKind() schema.ObjectKind {
	return schema.EmptyObjectKind
}

func (p GetObjectMetaAndKindPanicType) DeepCopyObject() runtime.Object {
	return &runtime.Unknown{}
}
