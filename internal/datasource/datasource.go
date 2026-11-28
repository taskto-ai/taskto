package datasource

import (
	"context"
	"errors"
	"slices"
)

type Kind string

const StorageUsage Kind = "storage_usage"

const ScopeViewer = "viewer"

var ErrUnavailable = errors.New("this data source isn't available")

type Spec struct {
	Scope   string
	Outputs map[string]string
}

func Specs() map[Kind]Spec {
	return map[Kind]Spec{StorageUsage: storageSpec}
}

func Lookup(kind string) (Spec, bool) {
	spec, ok := Specs()[Kind(kind)]
	return spec, ok
}

func Kinds() []string {
	out := []string{}
	for kind := range Specs() {
		out = append(out, string(kind))
	}
	slices.Sort(out)
	return out
}

type Viewer struct {
	UserID      int64
	WorkspaceID int64
}

type Reader interface {
	Read(ctx context.Context, v Viewer, kind Kind, outputs []string) (map[string]any, error)
}

type Sources struct {
	Storage StorageFunc
}

func (s Sources) Read(ctx context.Context, v Viewer, kind Kind, outputs []string) (map[string]any, error) {
	spec, ok := Specs()[kind]
	if !ok {
		return nil, ErrUnavailable
	}
	for _, name := range outputs {
		if _, known := spec.Outputs[name]; !known {
			return nil, ErrUnavailable
		}
	}
	var all map[string]any
	var err error
	switch kind {
	case StorageUsage:
		all, err = s.storage(ctx, v)
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(outputs))
	for _, name := range outputs {
		if value, present := all[name]; present {
			out[name] = value
		}
	}
	return out, nil
}
