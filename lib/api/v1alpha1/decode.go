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

// Objects is a config set: every Backend and ModelRoute read, with where
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

func (objs *Objects) decode(file string, b []byte) error {
	for i, doc := range documents(b) {
		var tm TypeMeta
		if err := yaml.Unmarshal(doc, &tm); err != nil {
			return fmt.Errorf("%s#%d: %w", file, i+1, err)
		}
		if tm.APIVersion == "" && tm.Kind == "" {
			continue // an empty document
		}
		if !strings.HasPrefix(tm.APIVersion, "golem.dev/") {
			continue
		}
		if tm.APIVersion != APIVersion {
			return fmt.Errorf("%s#%d: apiVersion %q: this version of golem reads %s", file, i+1, tm.APIVersion, APIVersion)
		}
		switch tm.Kind {
		case KindBackend:
			var o Backend
			if err := yaml.UnmarshalStrict(doc, &o); err != nil {
				return fmt.Errorf("%s#%d: Backend: %w", file, i+1, err)
			}
			objs.Backends = append(objs.Backends, Source[Backend]{o, file, i + 1})
		case KindModelRoute:
			var o ModelRoute
			if err := yaml.UnmarshalStrict(doc, &o); err != nil {
				return fmt.Errorf("%s#%d: ModelRoute: %w", file, i+1, err)
			}
			objs.Routes = append(objs.Routes, Source[ModelRoute]{o, file, i + 1})
		case KindLocalIdentity:
			var o LocalIdentity
			if err := yaml.UnmarshalStrict(doc, &o); err != nil {
				return fmt.Errorf("%s#%d: LocalIdentity: %w", file, i+1, err)
			}
			objs.Identities = append(objs.Identities, Source[LocalIdentity]{o, file, i + 1})
		case KindPriceList:
			var o PriceList
			if err := yaml.UnmarshalStrict(doc, &o); err != nil {
				return fmt.Errorf("%s#%d: PriceList: %w", file, i+1, err)
			}
			objs.Prices = append(objs.Prices, Source[PriceList]{o, file, i + 1})
		default:
			return fmt.Errorf("%s#%d: kind %q is not a golem config kind (Backend, ModelRoute, PriceList, LocalIdentity)", file, i+1, tm.Kind)
		}
	}
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
	h := sha256.New()
	err := fs.WalkDir(fsys, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ext := path.Ext(p); e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, err
}
