package main

import (
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

// clients are the control API's, every call with the caller's token.
func clients(ctx context.Context) (orgv1connect.OrgServiceClient, keysv1connect.KeyServiceClient, error) {
	s, err := loadSettings()
	if err != nil {
		return nil, nil, err
	}
	bearer := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			t, err := accessToken(ctx)
			if err != nil {
				return nil, err
			}
			r.Header().Set("Authorization", "Bearer "+t)
			return next(ctx, r)
		}
	}))
	return orgv1connect.NewOrgServiceClient(httpClient, s.API, bearer), keysv1connect.NewKeyServiceClient(httpClient, s.API, bearer), nil
}

// command parses a command's positional arguments (want of them, first)
// and then its flags.
func command(name string, args []string, want int, define func(*flag.FlagSet)) ([]string, *bool, error) {
	if len(args) < want || (want > 0 && strings.HasPrefix(args[0], "-")) {
		return nil, nil, usageError(fmt.Sprintf("%s takes %d argument(s) before its flags", name, want))
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "check the change, its authorization and the invariants; write nothing")
	define(fs)
	if err := fs.Parse(args[want:]); err != nil {
		return nil, nil, usageError(name + ": " + err.Error())
	}
	if fs.NArg() > 0 {
		return nil, nil, usageError(fmt.Sprintf("%s: unexpected %q", name, fs.Arg(0)))
	}
	return args[:want], dry, nil
}

func list(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
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

// apiCommand runs a command over the control API.
func apiCommand(ctx context.Context, args []string, out io.Writer) error {
	org, keys, err := clients(ctx)
	if err != nil {
		return err
	}
	done := func(dry bool, what string) {
		if dry {
			fmt.Fprintln(out, "ok (dry run, nothing written):", what)
		} else {
			fmt.Fprintln(out, what)
		}
	}
	switch noun, verb, rest := args[0], args[1], args[2:]; strings.TrimSpace(noun + " " + verb) {
	case "whoami":
		res, err := org.Whoami(ctx, connect.NewRequest(&orgv1.WhoamiRequest{}))
		if err != nil {
			return err
		}
		m := res.Msg
		tw := table(out)
		fmt.Fprintf(tw, "principal\t%s\n", m.Principal)
		fmt.Fprintf(tw, "kind\t%s\n", or(m.Kind, "(no principal yet)"))
		fmt.Fprintf(tw, "admin\t%v\n", m.Admin)
		fmt.Fprintf(tw, "member of\t%s\n", or(strings.Join(m.Scopes, ", "), "-"))
		fmt.Fprintf(tw, "manages\t%s\n", or(strings.Join(m.Manages, ", "), "-"))
		return tw.Flush()
	case "scopes list":
		if _, _, err := command("scopes list", rest, 0, func(*flag.FlagSet) {}); err != nil {
			return err
		}
		res, err := org.ListScopes(ctx, connect.NewRequest(&orgv1.ListScopesRequest{}))
		if err != nil {
			return err
		}
		for _, id := range res.Msg.Ids {
			fmt.Fprintln(out, id)
		}
	case "scopes get":
		pos, _, err := command("scopes get", rest, 1, func(*flag.FlagSet) {})
		if err != nil {
			return err
		}
		res, err := org.GetScope(ctx, connect.NewRequest(&orgv1.GetScopeRequest{Id: pos[0]}))
		if err != nil {
			return err
		}
		m, p := res.Msg, res.Msg.GetPolicy()
		tw := table(out)
		fmt.Fprintf(tw, "scope\t%s\n", m.Id)
		fmt.Fprintf(tw, "models\t%s\n", or(strings.Join(p.GetModels(), ", "), "-"))
		fmt.Fprintf(tw, "budget\t%d micros\n", p.GetBudgetMicros())
		fmt.Fprintf(tw, "members\t%s\n", or(strings.Join(m.Members, ", "), "-"))
		fmt.Fprintf(tw, "managers\t%s\n", or(strings.Join(m.Managers, ", "), "-"))
		return tw.Flush()
	case "keys list":
		var scope *string
		if _, _, err := command("keys list", rest, 0, func(fs *flag.FlagSet) {
			scope = fs.String("scope", "", "only this scope's keys")
		}); err != nil {
			return err
		}
		res, err := keys.ListKeys(ctx, connect.NewRequest(&keysv1.ListKeysRequest{Scope: *scope}))
		if err != nil {
			return err
		}
		tw := table(out)
		fmt.Fprintln(tw, "KEY\tOWNER\tSCOPE\tMODELS\tEXPIRES\tSTATE\tHASH")
		for _, k := range res.Msg.Keys {
			expires, state := "-", "active"
			if k.Expires != nil {
				expires = k.Expires.AsTime().Format(time.DateOnly)
			}
			if k.Revoked != nil {
				state = "revoked " + k.Revoked.AsTime().Format(time.DateOnly)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.Display, k.Owner, k.Scope, or(strings.Join(k.Models, ","), "(scope's)"),
				expires, state, k.Hash[:12])
		}
		return tw.Flush()
	case "scopes create":
		var models *string
		var budget *int64
		pos, dry, err := command("scopes create", rest, 1, func(fs *flag.FlagSet) {
			models = fs.String("models", "", "the route aliases its members may call, comma-separated")
			budget = fs.Int64("budget-micros", 0, "the period's budget in micro-units of the currency")
		})
		if err != nil {
			return err
		}
		if _, err := org.CreateScope(ctx, connect.NewRequest(&orgv1.CreateScopeRequest{Id: pos[0], ValidateOnly: *dry,
			Policy: &orgv1.ScopePolicy{Models: list(*models), BudgetMicros: *budget}})); err != nil {
			return err
		}
		done(*dry, "scope "+pos[0]+" created")
	case "members add":
		var kind, entra *string
		pos, dry, err := command("members add", rest, 2, func(fs *flag.FlagSet) {
			kind = fs.String("kind", "user", "user or service")
			entra = fs.String("entra", "", "the user's Entra object ID, or the service's app ID")
		})
		if err != nil {
			return err
		}
		if _, err := org.AddMember(ctx, connect.NewRequest(&orgv1.AddMemberRequest{Scope: pos[0], ValidateOnly: *dry,
			Member: &orgv1.Member{Id: pos[1], Kind: *kind, Entra: *entra}})); err != nil {
			return err
		}
		done(*dry, pos[1]+" is a member of "+pos[0])
	case "members remove":
		pos, dry, err := command("members remove", rest, 2, func(*flag.FlagSet) {})
		if err != nil {
			return err
		}
		if _, err := org.RemoveMember(ctx, connect.NewRequest(&orgv1.RemoveMemberRequest{Scope: pos[0], Principal: pos[1], ValidateOnly: *dry})); err != nil {
			return err
		}
		done(*dry, pos[1]+" removed from "+pos[0]+" (its keys there revoked)")
	case "managers add":
		pos, dry, err := command("managers add", rest, 2, func(*flag.FlagSet) {})
		if err != nil {
			return err
		}
		if _, err := org.BindManager(ctx, connect.NewRequest(&orgv1.BindManagerRequest{Scope: pos[0], Principal: pos[1], ValidateOnly: *dry})); err != nil {
			return err
		}
		done(*dry, pos[1]+" manages "+pos[0])
	case "keys create":
		var scope, owner, models, expires *string
		_, dry, err := command("keys create", rest, 0, func(fs *flag.FlagSet) {
			scope = fs.String("scope", "", "the scope the key calls in")
			owner = fs.String("owner", "", "the principal it acts as (default: you; a service's, if you manage the scope)")
			models = fs.String("models", "", "narrow the scope's models, comma-separated")
			expires = fs.String("expires", "", "when it expires, from now: 30d, 12h")
		})
		if err != nil {
			return err
		}
		if *scope == "" {
			return usageError("keys create needs --scope")
		}
		req := &keysv1.MintKeyRequest{Owner: *owner, Scope: *scope, Models: list(*models), ValidateOnly: *dry}
		if *expires != "" {
			d, err := duration(*expires)
			if err != nil || d <= 0 {
				return usageError("--expires: a duration such as 30d or 12h")
			}
			req.Expires = timestamppb.New(time.Now().Add(d))
		}
		res, err := keys.MintKey(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		if *dry {
			done(true, "a key in "+*scope)
			return nil
		}
		fmt.Fprintf(out, "%s\n\nThis is the key's only showing: golem keeps its hash, %s.\n", res.Msg.Key, res.Msg.Hash)
	case "keys revoke":
		pos, dry, err := command("keys revoke", rest, 1, func(*flag.FlagSet) {})
		if err != nil {
			return err
		}
		if _, err := keys.RevokeKey(ctx, connect.NewRequest(&keysv1.RevokeKeyRequest{Hash: pos[0], ValidateOnly: *dry})); err != nil {
			return err
		}
		done(*dry, "key "+pos[0]+" revoked")
	default:
		return usageError(fmt.Sprintf("no command %q", noun+" "+verb))
	}
	return nil
}

func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0) }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
