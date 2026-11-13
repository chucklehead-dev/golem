package compile

import (
	"cmp"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Default in-flight limits when a scope names none.
const (
	DefaultPrincipalInFlight = 4
	DefaultScopeInFlight     = 16
)

type adder func(obj, path, msg string, args ...any)

// entitlements compiles a LocalIdentity (standalone only). Without one, a
// standalone snapshot is implicit: one local principal, every route. A
// published snapshot's entitlements come from the state document.
func entitlements(objs v1.Objects, o Options, aliases map[string]string, add adder) snapshot.Entitlements {
	var e snapshot.Entitlements
	switch {
	case len(objs.Identities) == 0:
		e.Implicit = o.Standalone
		return e
	case !o.Standalone:
		for _, src := range objs.Identities {
			add("LocalIdentity/"+src.Object.Metadata.Name+" ("+src.Where()+")", "kind",
				"a LocalIdentity is for standalone mode only; entitlements come from the state document (ADR 0023)")
		}
		return e
	case len(objs.Identities) > 1:
		add("LocalIdentity/"+objs.Identities[1].Object.Metadata.Name+" ("+objs.Identities[1].Where()+")", "kind",
			"one LocalIdentity per config; merge the scopes and keys")
		return e
	}
	src := objs.Identities[0]
	obj := "LocalIdentity/" + src.Object.Metadata.Name + " (" + src.Where() + ")"
	scopes := map[string]*snapshot.Scope{}
	for i, sc := range src.Object.Spec.Scopes {
		at := fmt.Sprintf("spec.scopes[%d]", i)
		switch {
		case sc.Name == "":
			add(obj, at+".name", "required")
			continue
		case scopes[sc.Name] != nil:
			add(obj, at+".name", "another scope is named %q", sc.Name)
			continue
		case sc.BudgetMicros < 0:
			add(obj, at+".budgetMicros", "must not be negative")
		case sc.InFlight.Principal < 0 || sc.InFlight.Scope < 0:
			add(obj, at+".inFlight", "limits must not be negative")
		case sc.MaxOutputTokens < 0 || sc.MaxInputBytes < 0:
			add(obj, at, "ceilings must not be negative")
		case sc.Features != "" && sc.Features != "restricted" && sc.Features != "compatible":
			add(obj, at+".features", "%q: restricted or compatible (unset: restricted, compatible on agent routes)", sc.Features)
		case sc.ProviderStorage != "" && sc.ProviderStorage != "forbidden" && sc.ProviderStorage != "allowed":
			add(obj, at+".providerStorage", "%q: forbidden (the default) or allowed", sc.ProviderStorage)
		}
		for j, m := range sc.Models {
			if _, ok := aliases[m]; !ok {
				add(obj, fmt.Sprintf("%s.models[%d]", at, j), "no ModelRoute has alias %q", m)
			}
		}
		s := snapshot.Scope{ID: sc.Name, Models: slices.Sorted(slices.Values(sc.Models)), BudgetMicros: sc.BudgetMicros,
			Strict: sc.Strict, PrincipalInFlight: cmp.Or(sc.InFlight.Principal, DefaultPrincipalInFlight),
			ScopeInFlight: cmp.Or(sc.InFlight.Scope, DefaultScopeInFlight), StorageAllowed: sc.ProviderStorage == "allowed",
			MaxOutputTokens: sc.MaxOutputTokens, MaxInputBytes: sc.MaxInputBytes, Features: sc.Features}
		scopes[sc.Name] = &s
	}
	hashes := map[string]bool{}
	owners := map[string][]string{}
	for i, k := range src.Object.Spec.Keys {
		at := fmt.Sprintf("spec.keys[%d]", i)
		env, err := key.Parse(k.Key)
		switch {
		case err != nil:
			add(obj, at+".key", "%v", err)
			continue
		case env != key.Dev:
			add(obj, at+".key", "a standalone gateway takes only glm_dev_ keys (contracts.md §5); never put a live key in a file")
			continue
		case k.Principal == "":
			add(obj, at+".principal", "required")
		}
		sc := scopes[k.Scope]
		if sc == nil {
			add(obj, at+".scope", "no scope named %q", k.Scope)
			continue
		}
		for j, m := range k.Models {
			if !slices.Contains(sc.Models, m) {
				add(obj, fmt.Sprintf("%s.models[%d]", at, j), "scope %q does not allow %q: a key can only narrow its scope", k.Scope, m)
			}
		}
		h := key.Hash(k.Key)
		hx := hex.EncodeToString(h[:])
		if hashes[hx] {
			add(obj, at+".key", "the same key is listed twice")
			continue
		}
		hashes[hx] = true
		sk := snapshot.Key{Hash: hx, Display: key.Display(k.Key), Principal: k.Principal, PrincipalKind: "user",
			Scope: k.Scope, Models: slices.Sorted(slices.Values(k.Models)), Revoked: k.Revoked}
		if k.Expires != "" {
			t, err := time.Parse(time.RFC3339, k.Expires)
			if err != nil {
				add(obj, at+".expires", "not an RFC 3339 time: %v", err)
			}
			sk.ExpiresAt = t.UTC()
		}
		e.Keys = append(e.Keys, sk)
		owners[k.Principal] = append(owners[k.Principal], k.Scope)
	}
	// A LocalIdentity's keys name their principals: each is listed as a
	// key owner, a member of the scopes of its keys.
	for _, id := range slices.Sorted(maps.Keys(owners)) {
		e.KeyOwners = append(e.KeyOwners, snapshot.Owner{ID: id, Kind: "user", Scopes: slices.Compact(slices.Sorted(slices.Values(owners[id])))})
	}
	// One namespace for users and services: gateway/auth finds both by
	// their Entra ID, so the same ID twice would shadow one of them.
	seen := map[string]bool{}
	members := func(kind string, ms []v1.LocalMember) []snapshot.Member {
		var out []snapshot.Member
		for i, m := range ms {
			at := fmt.Sprintf("spec.%s[%d]", kind, i)
			switch {
			case m.Name == "":
				add(obj, at+".name", "required")
				continue
			case m.Entra == "":
				add(obj, at+".entra", "required: the user's Entra object ID, or the service's app ID")
				continue
			case seen[m.Entra]:
				add(obj, at+".entra", "%q is already a user or service here", m.Entra)
				continue
			case len(m.Scopes) == 0:
				add(obj, at+".scopes", "a member of no scope can call nothing")
				continue
			}
			seen[m.Entra] = true
			for j, sc := range m.Scopes {
				if scopes[sc] == nil {
					add(obj, fmt.Sprintf("%s.scopes[%d]", at, j), "no scope named %q", sc)
				}
			}
			def := cmp.Or(m.Default, m.Scopes[0])
			if !slices.Contains(m.Scopes, def) {
				add(obj, at+".default", "%q is not one of its scopes", def)
			}
			out = append(out, snapshot.Member{ID: m.Name, Entra: m.Entra, Scopes: slices.Sorted(slices.Values(m.Scopes)), Default: def})
		}
		slices.SortFunc(out, func(a, b snapshot.Member) int { return cmp.Compare(a.Entra, b.Entra) })
		return out
	}
	e.Users = members("users", src.Object.Spec.Users)
	e.Services = members("services", src.Object.Spec.Services)
	for _, sc := range scopes {
		e.Scopes = append(e.Scopes, *sc)
	}
	slices.SortFunc(e.Scopes, func(a, b snapshot.Scope) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(e.Keys, func(a, b snapshot.Key) int { return cmp.Compare(a.Hash, b.Hash) })
	return e
}

func priceList(objs v1.Objects, add adder) (string, []snapshot.Price) {
	var currency string
	var prices []snapshot.Price
	seen := map[string]bool{}
	for _, src := range objs.Prices {
		pl := src.Object
		obj := "PriceList/" + pl.Metadata.Name + " (" + src.Where() + ")"
		switch {
		case len(pl.Spec.Currency) != 3:
			add(obj, "spec.currency", "a three-letter currency code (ISO 4217)")
		case currency != "" && pl.Spec.Currency != currency:
			add(obj, "spec.currency", "%s, but another PriceList uses %s: one currency per installation", pl.Spec.Currency, currency)
		}
		currency = pl.Spec.Currency
		for i, p := range pl.Spec.Prices {
			at := fmt.Sprintf("spec.prices[%d]", i)
			switch {
			case p.Model == "":
				add(obj, at+".model", "required")
				continue
			case seen[p.Model]:
				add(obj, at+".model", "model %q is priced twice", p.Model)
				continue
			case p.InputPerMTok < 0 || p.OutputPerMTok < 0 || p.CacheReadPerMTok < 0 || p.CacheWritePerMTok < 0:
				add(obj, at, "rates must not be negative")
			}
			seen[p.Model] = true
			prices = append(prices, snapshot.Price(p))
		}
	}
	slices.SortFunc(prices, func(a, b snapshot.Price) int { return cmp.Compare(a.Model, b.Model) })
	return currency, prices
}
