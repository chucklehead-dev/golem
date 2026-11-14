package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Objects is a config set: every golem object read, by kind, with where
// each came from.
type Objects struct {
	Backends   []Source[Backend]
	Routes     []Source[ModelRoute]
	Identities []Source[LocalIdentity]
	Prices     []Source[PriceList]
}

// Source is an object and the file and document it was read from.
type Source[T any] struct {
	Object T
	File   string
	Doc    int // 1-based document number within File
}

// Where names an object's origin for an error message.
func (s Source[T]) Where() string { return fmt.Sprintf("%s#%d", s.File, s.Doc) }

// DecodeFS reads every .yaml and .yml file of fsys, recursively, in name
// order. Unknown fields are errors (a misspelt field must not be ignored),
// as are kinds other than golem's; objects of other API groups (a
// kustomization, a Secret) are skipped.
func DecodeFS(fsys fs.FS) (Objects, error) {
	var objs Objects
	files, err := yamlFiles(fsys)
	if err != nil {
		return objs, err
	}
	slices.Sort(files)
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return objs, err
		}
		if err := objs.decode(f, b); err != nil {
			return objs, err
		}
	}
	return objs, nil
}

// yamlFiles lists fsys's YAML files in fs.WalkDir's order.
func yamlFiles(fsys fs.FS) ([]string, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ext := path.Ext(p); !d.IsDir() && (ext == ".yaml" || ext == ".yml") {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

func (objs *Objects) decode(file string, b []byte) error {
	for i, doc := range documents(b) {
		n := i + 1
		var tm TypeMeta
		if err := yaml.Unmarshal(doc, &tm); err != nil {
			return fmt.Errorf("%s#%d: %w", file, n, err)
		}
		if tm.APIVersion == "" && tm.Kind == "" {
			continue // an empty document
		}
		if !strings.HasPrefix(tm.APIVersion, "golem.dev/") {
			continue
		}
		if tm.APIVersion != APIVersion {
			return fmt.Errorf("%s#%d: apiVersion %q: this version of golem reads %s", file, n, tm.APIVersion, APIVersion)
		}
		var err error
		switch tm.Kind {
		case KindBackend:
			err = decodeInto(&objs.Backends, doc, file, n)
		case KindModelRoute:
			err = decodeInto(&objs.Routes, doc, file, n)
		case KindLocalIdentity:
			err = decodeInto(&objs.Identities, doc, file, n)
		case KindPriceList:
			err = decodeInto(&objs.Prices, doc, file, n)
		default:
			return fmt.Errorf("%s#%d: kind %q is not a golem config kind (Backend, ModelRoute, PriceList, LocalIdentity)", file, n, tm.Kind)
		}
		if err != nil {
			return fmt.Errorf("%s#%d: %s: %w", file, n, tm.Kind, err)
		}
	}
	return nil
}

// decodeInto decodes doc strictly, refusing unknown fields, and appends it
// to list with where it was read.
func decodeInto[T any](list *[]Source[T], doc []byte, file string, n int) error {
	var o T
	if err := yaml.UnmarshalStrict(doc, &o); err != nil {
		return err
	}
	*list = append(*list, Source[T]{o, file, n})
	return nil
}

// documents splits a YAML stream on its "---" separators.
func documents(b []byte) [][]byte {
	var docs [][]byte
	var cur bytes.Buffer
	for line := range bytes.Lines(b) {
		if t := bytes.TrimRight(line, " \t\r\n"); bytes.Equal(t, []byte("---")) {
			docs = append(docs, bytes.Clone(cur.Bytes()))
			cur.Reset()
			continue
		}
		cur.Write(line)
	}
	return append(docs, cur.Bytes())
}

// ContentHash hashes every YAML file's name and contents, as DecodeFS reads
// them: a cheap test of whether a config directory changed.
func ContentHash(fsys fs.FS) ([32]byte, error) {
	var sum [32]byte
	files, err := yamlFiles(fsys)
	if err != nil {
		return sum, err
	}
	h := sha256.New()
	for _, p := range files {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return sum, err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}
