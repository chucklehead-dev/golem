// Package snapshot is the format of what the gateway runs on: backends,
// routes, entitlements and prices, compiled from the config CRDs and the
// state (ADR 0006), and the signed lease that bounds how long it may be used
// (ADR 0030). It is data only; lib/compile builds it, the gateway compiles
// it into lookup structures.
//
// The encoding is JSON in this first version; the design's protobuf
// encoding (ADR 0022) replaces it with the proto skeletons, behind the same
// Encode and Decode.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"strconv"
	"time"
)

// Schema is this format's version; a reader refuses any other.
const Schema = 1

// MaxMicros bounds a budget, and the cost one call adds to a counter, in
// micro-units (2^50: about 1.1 billion units of currency). Valkey's scripts
// sum in Lua numbers, doubles, exact only below 2^53: under this bound a
// period's sums stay exact there as they are in memory.
const MaxMicros = 1 << 50

// Snapshot is one pool's complete configuration at one moment.
type Snapshot struct {
	Schema int `json:"schema"`
	// PublishSeq orders publications; a gateway never applies one at or
	// below its floor (model/publication.md).
	PublishSeq uint64    `json:"publishSeq"`
	IssuedAt   time.Time `json:"issuedAt"`
	// ValidUntil is the authorization lease: no new request is served
	// after it. Zero only in standalone mode, which has no publisher.
	ValidUntil time.Time `json:"validUntil,omitzero"`
	Source     Source    `json:"source"`
	Backends   []Backend `json:"backends"`
	Routes     []Route   `json:"routes"`
	// Entitlements are who may call what, within which limits (the state
	// document's, org-model.md; in standalone, LocalIdentity's).
	Entitlements Entitlements `json:"entitlements"`
	Currency     string       `json:"currency,omitempty"`
	Prices       []Price      `json:"prices,omitempty"`
}

// Entitlements are a snapshot's principals' rights.
type Entitlements struct {
	// Implicit (standalone only, without a LocalIdentity): every request is
	// the local principal in the local scope, which may call every route
	// with no limits (ADR 0023).
	Implicit bool    `json:"implicit,omitempty"`
	Scopes   []Scope `json:"scopes,omitempty"`
	Keys     []Key   `json:"keys,omitempty"`
	// Users and Services are the principals Entra tokens name: a user by
	// its object ID, a service by its app (client) ID (ADR 0009).
	Users    []Member `json:"users,omitempty"`
	Services []Member `json:"services,omitempty"`
	// KeyOwners are the principals that hold keys but no Entra identity
	// (a standalone LocalIdentity's). A key is honoured only for a listed
	// owner (a user, a service or one of these) of its kind, a member of
	// its scope (golem#5).
	KeyOwners []Owner `json:"keyOwners,omitempty"`
}

// Owner is a principal that holds keys: its kind and memberships.
type Owner struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"` // user or service
	Scopes []string `json:"scopes"`
}

// Member is a principal and the scopes it may act in.
type Member struct {
	ID      string   `json:"id"`      // golem's principal ID
	Entra   string   `json:"entra"`   // the user's oid, or the service's app ID
	Scopes  []string `json:"scopes"`  // memberships
	Default string   `json:"default"` // the scope used without a golem-scope header
}

// Scope is a scope's rights and limits for the period.
type Scope struct {
	ID           string   `json:"id"`
	Models       []string `json:"models"` // aliases its members may call
	BudgetMicros int64    `json:"budgetMicros,omitempty"`
	// DefaultMemberCapMicros is a member's cap without one of its own
	// (lib/api's); Members are the members with a cap of their own or
	// spend this period, each with its cap and known spend (org-model.md
	// §4.2). Zero is none.
	DefaultMemberCapMicros int64 `json:"defaultMemberCapMicros,omitempty"`
	// MemberRPM and MemberTPM are each member's rate limit, ScopeRPM and
	// ScopeTPM the whole scope's (lib/api's RateLimits); 0: none.
	MemberRPM int64           `json:"memberRPM,omitempty"`
	MemberTPM int64           `json:"memberTPM,omitempty"`
	ScopeRPM  int64           `json:"scopeRPM,omitempty"`
	ScopeTPM  int64           `json:"scopeTPM,omitempty"`
	Members   []MemberAccount `json:"members,omitempty"`
	// BudgetMode is what the budget does at admission (PRD §9.1, ADR 0050):
	// "observe" (admitted, the ledger row marked over budget), "warn"
	// (admitted and marked, with a golem-warning header) or "enforce"
	// (refused). The compiler sets it for every scope (its caps follow it); a
	// gateway reads any other value as enforce, failing closed.
	BudgetMode             string `json:"budgetMode,omitempty"`
	DeclaredWorstCaseCheck bool   `json:"declaredWorstCaseCheck,omitempty"` // ADR 0044
	PrincipalInFlight      int    `json:"principalInFlight"`
	ScopeInFlight          int    `json:"scopeInFlight"`
	// The scope's request policy: provider-side storage allowed (ADR 0027),
	// and ceilings on output tokens asked for and on input size (ADR 0038;
	// zero is none).
	StorageAllowed  bool  `json:"storageAllowed,omitempty"`
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
	MaxInputBytes   int64 `json:"maxInputBytes,omitempty"`
	// Features: "restricted", "compatible", or "" (by the route's kind).
	Features string `json:"features,omitempty"`
	// KnownSpend is the ledger's spend for Period (a calendar month,
	// "2026-11") of the calls that ended before Since, a minute
	// golem-control places an ingestion allowance behind the snapshot's
	// time; the site counter's minutes from Since on add the rest
	// (ADR 0048). A gateway counts a snapshot of another period as none: a
	// new month starts from nothing.
	Period     string    `json:"period,omitempty"`
	KnownSpend int64     `json:"knownSpend,omitempty"`
	Since      time.Time `json:"since,omitzero"`
}

// MemberAccount is a member's account in a scope: its own cap (0: the
// scope's default) and its known spend this period, in the scope's Period
// and from its Since, as the scope's KnownSpend is.
type MemberAccount struct {
	Principal  string `json:"principal"`
	CapMicros  int64  `json:"capMicros,omitempty"`
	KnownSpend int64  `json:"knownSpend,omitempty"`
}

// MemberCap is principal's cap in s: its own, else the scope's default; 0
// is none.
func (s *Scope) MemberCap(principal string) int64 {
	for _, m := range s.Members {
		if m.Principal == principal && m.CapMicros > 0 {
			return m.CapMicros
		}
	}
	return s.DefaultMemberCapMicros
}

// Key is a key's entitlement: by hash, never the key itself.
type Key struct {
	Hash          string    `json:"hash"`    // hex SHA-256
	Display       string    `json:"display"` // the shown prefix
	Principal     string    `json:"principal"`
	PrincipalKind string    `json:"principalKind"` // user or service
	Scope         string    `json:"scope"`
	Models        []string  `json:"models,omitempty"` // narrows the scope's
	ExpiresAt     time.Time `json:"expiresAt,omitzero"`
	Revoked       bool      `json:"revoked,omitempty"`
	// CapMicros is the key's spend cap for the period (0: none), and
	// KnownSpend its known spend, in its scope's Period and from its Since.
	CapMicros  int64 `json:"capMicros,omitempty"`
	KnownSpend int64 `json:"knownSpend,omitempty"`
}

// Price is a model's rates in micro-units per million tokens.
type Price struct {
	Model             string `json:"model"`
	InputPerMTok      int64  `json:"inputPerMTok"`
	OutputPerMTok     int64  `json:"outputPerMTok"`
	CacheReadPerMTok  int64  `json:"cacheReadPerMTok,omitempty"`
	CacheWritePerMTok int64  `json:"cacheWritePerMTok,omitempty"`
}

// Source names the inputs a snapshot was built from (model/freshness.md).
type Source struct {
	StateVersion   int64  `json:"stateVersion"`
	ConfigRevision int64  `json:"configRevision"`
	ConfigDigest   string `json:"configDigest"`
}

// Backend is a backend as compiled: its secrets are references, never values
// (ADR 0005).
type Backend struct {
	Name    string `json:"name"`
	Dialect string `json:"dialect"`
	// Envelope is "" or "bedrock" (lib/api's BackendSpec.Envelope).
	Envelope    string       `json:"envelope,omitempty"`
	Auth        Auth         `json:"auth"`
	Deployments []Deployment `json:"deployments"`
	// AllowPrivate grants loopback and private addresses (golem#5).
	AllowPrivate bool `json:"allowPrivate,omitempty"`
}

// Auth is a header and where its secret comes from.
type Auth struct {
	Header    string `json:"header"`
	Scheme    string `json:"scheme,omitempty"`
	SecretRef string `json:"secretRef,omitempty"` // "name/key"
	Env       string `json:"env,omitempty"`
}

// Deployment is one model at one endpoint.
type Deployment struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Model string `json:"model"`
	// Region is a bedrock deployment's region, its backend's filled in.
	Region string `json:"region,omitempty"`
	// MaxInFlight is each gateway's ceiling on the deployment's calls in
	// flight; 0: none.
	MaxInFlight int `json:"maxInFlight,omitempty"`
	// MaxInFlightPerPrincipal is each gateway's ceiling on one principal's
	// calls in flight on the deployment; 0: none.
	MaxInFlightPerPrincipal int `json:"maxInFlightPerPrincipal,omitempty"`
	// HealthPath and HealthIntervalMs are the deployment's active check
	// (lib/api's HealthCheck), its default filled in; "": none.
	HealthPath       string `json:"healthPath,omitempty"`
	HealthIntervalMs int64  `json:"healthIntervalMs,omitempty"`
	// QuotaRPM is the site's share of the provider's quota, requests a
	// minute (lib/api's Quota); 0: none.
	QuotaRPM int64 `json:"quotaRPM,omitempty"`
	// Betas are the anthropic-beta values it accepts; nil: any.
	Betas []string `json:"betas,omitempty"`
}

// Route is an alias and its pool.
type Route struct {
	Alias   string   `json:"alias"`
	Dialect string   `json:"dialect"`
	Pool    []Target `json:"pool"`
	// Agent and ApprovedBetas are the ModelRoute's (spec.agent,
	// spec.approvedBetas).
	Agent         bool     `json:"agent,omitempty"`
	ApprovedBetas []string `json:"approvedBetas,omitempty"`
	// The call's bounds in milliseconds, the compiler's defaults filled in
	// (ModelRoute spec.timeouts).
	MaxDurationMs int64 `json:"maxDurationMs"`
	HeadersMs     int64 `json:"headersMs"`
	IdleMs        int64 `json:"idleMs"`
	// MaxAttempts is the most attempts a call makes before it commits,
	// the compiler's default filled in (ModelRoute spec.retries).
	MaxAttempts int `json:"maxAttempts"`
}

// Target is a pool member.
type Target struct {
	Backend    string `json:"backend"`
	Deployment string `json:"deployment"`
	Weight     int    `json:"weight"`
}

// Encode returns the snapshot's canonical bytes: compile sorts everything,
// and encoding/json orders struct fields, so equal snapshots encode equally.
func (s *Snapshot) Encode() ([]byte, error) { return json.Marshal(s) }

// Decode reads a snapshot, refusing a schema this reader does not know,
// and any member it does not know, a duplicate name, or a name in another
// case. A snapshot is policy: a member a reader skipped is policy it would
// not apply (a renamed check read as off), so it fails closed instead.
func Decode(b []byte) (*Snapshot, error) {
	// The schema first: a newer schema's members are unknown here, and the
	// refusal should say to upgrade the gateway, not name a member.
	var head struct {
		Schema int `json:"schema"`
	}
	if err := jsonv2.Unmarshal(b, &head); err != nil {
		return nil, err
	}
	if head.Schema != Schema {
		return nil, &SchemaError{Got: head.Schema}
	}
	var s Snapshot
	if err := jsonv2.Unmarshal(b, &s, jsonv2.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	return &s, nil
}

// SchemaError is a snapshot of a schema this gateway cannot read.
type SchemaError struct{ Got int }

func (e *SchemaError) Error() string {
	return "snapshot schema " + strconv.Itoa(e.Got) + ", this gateway reads " + strconv.Itoa(Schema) + ": upgrade the gateway before the publisher"
}

// Hash is the hex SHA-256 of encoded bytes: a snapshot's name in the bucket
// and in status.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
