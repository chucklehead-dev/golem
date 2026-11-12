// Command golemctl is golem's CLI (tools.md). This first version has the
// offline config check CI runs on the GitOps repository:
//
//	golemctl config validate ./config
//	golemctl config validate --standalone ./config
//
// It runs the same snapshot compiler as golem-control and the standalone
// gateway (ADR 0005: one validation path).
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/compile"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

const usage = `usage: golemctl config validate [--standalone] <dir>

  Checks the golem CRD YAML in <dir> (recursively) as golem-control would,
  and prints every problem with its file, object and field.`

func main() {
	if len(os.Args) < 3 || os.Args[1] != "config" || os.Args[2] != "validate" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	standalone := fs.Bool("standalone", false, "validate for standalone mode (environment credentials, plain http)")
	fs.Parse(os.Args[3:])
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(validate(fs.Arg(0), *standalone))
}

func validate(dir string, standalone bool) int {
	root, err := os.OpenRoot(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golemctl:", err)
		return 2
	}
	defer root.Close()
	objs, err := v1.DecodeFS(root.FS())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	o := compile.Options{Standalone: standalone, Now: time.Now(), PublishSeq: 1}
	if !standalone {
		o.Lease = 4 * time.Hour // validation does not depend on its length
	}
	s, err := compile.Compile(objs, o)
	var ps compile.Problems
	if errors.As(err, &ps) {
		for _, p := range ps {
			fmt.Println(p)
		}
		fmt.Fprintf(os.Stderr, "%d problem(s)\n", len(ps))
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	b, _ := s.Encode()
	fmt.Printf("ok: %d backends, %d routes; config digest %s, snapshot %s\n",
		len(s.Backends), len(s.Routes), s.Source.ConfigDigest[:12], snapshot.Hash(b)[:12])
	return 0
}
