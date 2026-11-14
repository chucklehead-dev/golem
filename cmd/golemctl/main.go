// Command golemctl is golem's CLI (tools.md): the offline config check CI
// runs on the GitOps repository, sign-in, and the org and key commands
// over the control API.
//
//	golemctl config validate [--standalone] ./config
//	golemctl login --tenant <id> --client-id <id> --api https://golem-control.example
//	golemctl scopes create team-a --models claude,gpt
//	golemctl members add team-a alice --kind user --entra <oid>
//	golemctl keys create --scope team-a --expires 30d
//
// config validate runs the same snapshot compiler as golem-control and the
// standalone gateway (ADR 0005: one validation path).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/compile"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

const usage = `usage:
  golemctl config validate [--standalone] <dir>
      Checks the golem CRD YAML in <dir> (recursively) as golem-control would.
  golemctl keys mint --dev
      Prints a new glm_dev_ key, for a standalone gateway's LocalIdentity.
  golemctl login --tenant <id> --client-id <id> --api <url> [--audience api://golem]
      Signs in with Entra (device code) and saves where golem is.
  golemctl token
      Prints a current Entra access token for golem (for agents' credential helpers).
  golemctl scopes create <id> --models a,b [--budget-micros n] [--dry-run]
  golemctl members add <scope> <principal> --kind user|service [--entra <id>] [--dry-run]
  golemctl members remove <scope> <principal> [--dry-run]
  golemctl managers add <scope> <principal> [--dry-run]
  golemctl keys create --scope <scope> [--owner <principal>] [--models a,b] [--expires 30d] [--dry-run]
  golemctl keys revoke <hash> [--dry-run]`

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

// run is golemctl: its exit status.
func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) < 2 && !(len(args) == 1 && (args[0] == "token" || args[0] == "login")) {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	var err error
	switch cmd := args[0]; {
	case cmd == "config" && args[1] == "validate":
		return validate(args[2:], out, errOut)
	case cmd == "keys" && args[1] == "mint":
		return mint(args[2:], out, errOut)
	case cmd == "login":
		err = login(ctx, args[1:], out, errOut)
	case cmd == "token":
		var t string
		if t, err = accessToken(ctx); err == nil {
			fmt.Fprintln(out, t)
		}
	default:
		err = apiCommand(ctx, args, out)
	}
	var u usageError
	switch {
	case errors.As(err, &u):
		fmt.Fprintln(errOut, "golemctl:", err)
		fmt.Fprintln(errOut, usage)
		return 2
	case err != nil:
		fmt.Fprintln(errOut, "golemctl:", err)
		return 1
	}
	return 0
}

// usageError is a command used wrongly.
type usageError string

func (u usageError) Error() string { return string(u) }

// mint prints a new dev key. Live keys are minted by golem-control
// (golemctl keys create), which stores only their hash.
func mint(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("mint", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dev := fs.Bool("dev", false, "a glm_dev_ key for a standalone gateway")
	if fs.Parse(args) != nil {
		return 2
	}
	if !*dev {
		fmt.Fprintln(errOut, "golemctl: live keys are minted by golem-control: golemctl keys create --scope <scope>; for a standalone gateway, golemctl keys mint --dev")
		return 2
	}
	fmt.Fprintln(out, key.New(key.Dev))
	return 0
}

func validate(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	standalone := fs.Bool("standalone", false, "validate for standalone mode (environment credentials, plain http)")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	root, err := os.OpenRoot(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(errOut, "golemctl:", err)
		return 2
	}
	defer root.Close()
	objs, err := v1.DecodeFS(root.FS())
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	o := compile.Options{Standalone: *standalone, Now: time.Now(), PublishSeq: 1}
	if !*standalone {
		o.Lease = 4 * time.Hour // validation does not depend on its length
	}
	s, err := compile.Compile(objs, o)
	var ps compile.Problems
	if errors.As(err, &ps) {
		for _, p := range ps {
			fmt.Fprintln(out, p)
		}
		fmt.Fprintf(errOut, "%d problem(s)\n", len(ps))
		return 1
	}
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	b, _ := s.Encode()
	fmt.Fprintf(out, "ok: %d backends, %d routes; config digest %s, snapshot %s\n",
		len(s.Backends), len(s.Routes), s.Source.ConfigDigest[:12], snapshot.Hash(b)[:12])
	return 0
}
