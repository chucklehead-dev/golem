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
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

const usage = `usage:
  golemctl config validate [--standalone] <dir>
      Checks the golem CRD YAML in <dir> (recursively) as golem-control would,
      and prints every problem with its file, object and field.
  golemctl keys mint --dev
      Prints a new glm_dev_ key, for a standalone gateway's LocalIdentity.`

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "keys" && os.Args[2] == "mint" {
		os.Exit(mint(os.Args[3:]))
	}
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

// mint prints a new dev key. Live keys are minted by golem-control, which
// stores only their hash (control-plane.md §5).
func mint(args []string) int {
	fs := flag.NewFlagSet("mint", flag.ExitOnError)
	dev := fs.Bool("dev", false, "a glm_dev_ key for a standalone gateway")
	fs.Parse(args)
	if !*dev {
		fmt.Fprintln(os.Stderr, "golemctl: live keys are minted by golem-control (not built yet); for a standalone gateway, golemctl keys mint --dev")
		return 2
	}
	fmt.Println(key.New(key.Dev))
	return 0
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
