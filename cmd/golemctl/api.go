package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	keysv1 "github.com/chucklehead-dev/golem/gen/golem/keys/v1"
	"github.com/chucklehead-dev/golem/gen/golem/keys/v1/keysv1connect"
	orgv1 "github.com/chucklehead-dev/golem/gen/golem/org/v1"
	"github.com/chucklehead-dev/golem/gen/golem/org/v1/orgv1connect"
)

// call is one run of a command over the control API: the clients, the
// command's positional arguments, its --dry-run, and where it prints.
type call struct {
	org  orgv1connect.OrgServiceClient
	keys keysv1connect.KeyServiceClient
	pos  []string
	dry  bool
	out  io.Writer
}

// done reports a change made, or, in a dry run, one that would be.
func (c *call) done(what string) {
	if c.dry {
		fmt.Fprintln(c.out, "ok (dry run, nothing written):", what)
	} else {
		fmt.Fprintln(c.out, what)
	}
}

// apiCommand is a command over the control API.
type apiCommand struct {
	args int // positional arguments, which come before the flags
	// flags defines the command's own flags on fs and returns what runs
	// the command once they are parsed.
	flags func(fs *flag.FlagSet) func(context.Context, *call) error
}

// noFlags is a command with no flags of its own.
func noFlags(run func(context.Context, *call) error) func(*flag.FlagSet) func(context.Context, *call) error {
	return func(*flag.FlagSet) func(context.Context, *call) error { return run }
}

// apiCommands are golemctl's commands over the control API, by name.
var apiCommands = map[string]apiCommand{ // read-only
	"whoami": {flags: noFlags(func(ctx context.Context, c *call) error {
		res, err := c.org.Whoami(ctx, connect.NewRequest(&orgv1.WhoamiRequest{}))
		if err != nil {
			return err
		}
		m := res.Msg
		return rows(c.out,
			"principal", m.Principal,
			"kind", cmp.Or(m.Kind, "(no principal yet)"),
			"admin", strconv.FormatBool(m.Admin),
			"member of", joined(m.Scopes, ", ", "-"),
			"manages", joined(m.Manages, ", ", "-"))
	})},
	"scopes list": {flags: noFlags(func(ctx context.Context, c *call) error {
		res, err := c.org.ListScopes(ctx, connect.NewRequest(&orgv1.ListScopesRequest{}))
		if err != nil {
			return err
		}
		for _, id := range res.Msg.Ids {
			fmt.Fprintln(c.out, id)
		}
		return nil
	})},
	"scopes get": {args: 1, flags: noFlags(func(ctx context.Context, c *call) error {
		res, err := c.org.GetScope(ctx, connect.NewRequest(&orgv1.GetScopeRequest{Id: c.pos[0]}))
		if err != nil {
			return err
		}
		m, p := res.Msg, res.Msg.GetPolicy()
		return rows(c.out,
			"scope", m.Id,
			"models", joined(p.GetModels(), ", ", "-"),
			"budget", fmt.Sprintf("%d micros", p.GetBudgetMicros()),
			"members", joined(m.Members, ", ", "-"),
			"managers", joined(m.Managers, ", ", "-"))
	})},
	"keys list": {flags: func(fs *flag.FlagSet) func(context.Context, *call) error {
		scope := fs.String("scope", "", "only this scope's keys")
		return func(ctx context.Context, c *call) error {
			res, err := c.keys.ListKeys(ctx, connect.NewRequest(&keysv1.ListKeysRequest{Scope: *scope}))
			if err != nil {
				return err
			}
			tw := table(c.out)
			fmt.Fprintln(tw, "KEY\tOWNER\tSCOPE\tMODELS\tEXPIRES\tSTATE\tHASH")
			for _, k := range res.Msg.Keys {
				expires, state := "-", "active"
				if k.Expires != nil {
					expires = k.Expires.AsTime().Format(time.DateOnly)
				}
				if k.Revoked != nil {
					state = "revoked " + k.Revoked.AsTime().Format(time.DateOnly)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.Display, k.Owner, k.Scope,
					joined(k.Models, ",", "(scope's)"), expires, state, k.Hash[:12])
			}
			return tw.Flush()
		}
	}},
	"scopes create": {args: 1, flags: func(fs *flag.FlagSet) func(context.Context, *call) error {
		models := fs.String("models", "", "the route aliases its members may call, comma-separated")
		budget := fs.Int64("budget-micros", 0, "the period's budget in micro-units of the currency")
		return func(ctx context.Context, c *call) error {
			if _, err := c.org.CreateScope(ctx, connect.NewRequest(&orgv1.CreateScopeRequest{Id: c.pos[0], ValidateOnly: c.dry,
				Policy: &orgv1.ScopePolicy{Models: splitList(*models), BudgetMicros: *budget}})); err != nil {
				return err
			}
			c.done("scope " + c.pos[0] + " created")
			return nil
		}
	}},
	"members add": {args: 2, flags: func(fs *flag.FlagSet) func(context.Context, *call) error {
		kind := fs.String("kind", "user", "user or service")
		entra := fs.String("entra", "", "the user's Entra object ID, or the service's app ID")
		return func(ctx context.Context, c *call) error {
			if _, err := c.org.AddMember(ctx, connect.NewRequest(&orgv1.AddMemberRequest{Scope: c.pos[0], ValidateOnly: c.dry,
				Member: &orgv1.Member{Id: c.pos[1], Kind: *kind, Entra: *entra}})); err != nil {
				return err
			}
			c.done(c.pos[1] + " is a member of " + c.pos[0])
			return nil
		}
	}},
	"members remove": {args: 2, flags: noFlags(func(ctx context.Context, c *call) error {
		if _, err := c.org.RemoveMember(ctx, connect.NewRequest(&orgv1.RemoveMemberRequest{Scope: c.pos[0], Principal: c.pos[1],
			ValidateOnly: c.dry})); err != nil {
			return err
		}
		c.done(c.pos[1] + " removed from " + c.pos[0] + " (its keys there revoked)")
		return nil
	})},
	"managers add": {args: 2, flags: noFlags(func(ctx context.Context, c *call) error {
		if _, err := c.org.BindManager(ctx, connect.NewRequest(&orgv1.BindManagerRequest{Scope: c.pos[0], Principal: c.pos[1],
			ValidateOnly: c.dry})); err != nil {
			return err
		}
		c.done(c.pos[1] + " manages " + c.pos[0])
		return nil
	})},
	"keys create": {flags: func(fs *flag.FlagSet) func(context.Context, *call) error {
		scope := fs.String("scope", "", "the scope the key calls in")
		owner := fs.String("owner", "", "the principal it acts as (default: you; a service's, if you manage the scope)")
		models := fs.String("models", "", "narrow the scope's models, comma-separated")
		expires := fs.String("expires", "", "when it expires, from now: 30d, 12h")
		return func(ctx context.Context, c *call) error {
			if *scope == "" {
				return usageError("keys create needs --scope")
			}
			req := &keysv1.MintKeyRequest{Owner: *owner, Scope: *scope, Models: splitList(*models), ValidateOnly: c.dry}
			if *expires != "" {
				d, err := duration(*expires)
				if err != nil || d <= 0 {
					return usageError("--expires: a duration such as 30d or 12h")
				}
				req.Expires = timestamppb.New(time.Now().Add(d))
			}
			res, err := c.keys.MintKey(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if c.dry {
				c.done("a key in " + *scope)
				return nil
			}
			fmt.Fprintf(c.out, "%s\n\nThis is the key's only showing: golem keeps its hash, %s.\n", res.Msg.Key, res.Msg.Hash)
			return nil
		}
	}},
	"keys revoke": {args: 1, flags: noFlags(func(ctx context.Context, c *call) error {
		if _, err := c.keys.RevokeKey(ctx, connect.NewRequest(&keysv1.RevokeKeyRequest{Hash: c.pos[0], ValidateOnly: c.dry})); err != nil {
			return err
		}
		c.done("key " + c.pos[0] + " revoked")
		return nil
	})},
}

// runAPI runs a command over the control API: args's first two words
// name it (one, for whoami), and the rest are its arguments and flags.
func runAPI(ctx context.Context, args []string, out io.Writer) error {
	org, keys, err := clients()
	if err != nil {
		return err
	}
	n := min(2, len(args))
	name := strings.Join(args[:n], " ")
	cmd, ok := apiCommands[name]
	if !ok {
		return usageError(fmt.Sprintf("no command %q", name))
	}
	c := &call{org: org, keys: keys, out: out}
	run, err := parse(name, args[n:], cmd, c)
	if err != nil {
		return err
	}
	return run(ctx, c)
}

// parse reads a command's positional arguments (cmd.args of them, first)
// and then its flags into c, and returns what runs it. Every command takes
// --dry-run; only the mutating ones send it.
func parse(name string, args []string, cmd apiCommand, c *call) (func(context.Context, *call) error, error) {
	if len(args) < cmd.args || (cmd.args > 0 && strings.HasPrefix(args[0], "-")) {
		return nil, usageError(fmt.Sprintf("%s takes %d argument(s) before its flags", name, cmd.args))
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&c.dry, "dry-run", false, "check the change, its authorization and the invariants; write nothing")
	run := cmd.flags(fs)
	if err := fs.Parse(args[cmd.args:]); err != nil {
		return nil, usageError(name + ": " + err.Error())
	}
	if fs.NArg() > 0 {
		return nil, usageError(fmt.Sprintf("%s: unexpected %q", name, fs.Arg(0)))
	}
	c.pos = args[:cmd.args]
	return run, nil
}

// clients are the control API's, every call with the caller's token.
func clients() (orgv1connect.OrgServiceClient, keysv1connect.KeyServiceClient, error) {
	s, err := loadSettings()
	if err != nil {
		return nil, nil, err
	}
	bearer := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			t, err := accessToken(ctx, control)
			if err != nil {
				return nil, err
			}
			r.Header().Set("Authorization", "Bearer "+t)
			return next(ctx, r)
		}
	}))
	return orgv1connect.NewOrgServiceClient(httpClient, s.API, bearer), keysv1connect.NewKeyServiceClient(httpClient, s.API, bearer), nil
}

// splitList is a comma-separated flag's values, blanks dropped.
func splitList(s string) []string {
	var out []string
	for x := range strings.SplitSeq(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// duration reads a Go duration or a number of days ("30d").
func duration(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		return time.Duration(n) * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0) }

// rows prints label, value pairs as an aligned two-column table.
func rows(w io.Writer, kv ...string) error {
	tw := table(w)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(tw, "%s\t%s\n", kv[i], kv[i+1])
	}
	return tw.Flush()
}

// joined is xs joined by sep, or none if there are none.
func joined(xs []string, sep, none string) string { return cmp.Or(strings.Join(xs, sep), none) }
