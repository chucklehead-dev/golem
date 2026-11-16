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
	"fmt"
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
	// PeriodZone is the time zone (an IANA name) the publisher's budget
	// periods are calendar months in (golem-control's -period-zone): known
	// spend's Period is a month there. A gateway whose own -period-zone
	// differs refuses the snapshot, naming both, since its admission and
	// ledger rows would put a call near midnight in another month. Empty:
	// not stated (standalone, an older publisher), not checked.
	PeriodZone string    `json:"periodZone,omitempty"`
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
	// MemberRPM is each member's rate limit and ScopeRPM the whole scope's,
	// in requests a minute (lib/api's RateLimits); 0: none.
	MemberRPM int64 `json:"memberRPM,omitempty"`
	ScopeRPM  int64 `json:"scopeRPM,omitempty"`
	// BudgetMode is what the budget does at admission (PRD §9.1, ADR 0050):
	// "observe" (admitted, the ledger row marked over budget), "warn"
	// (admitted and marked, with a golem-warning header) or "enforce"
	// (refused). The compiler sets it for every scope; a gateway reads any
	// other value as enforce, failing closed.
	BudgetMode        string `json:"budgetMode,omitempty"`
	PrincipalInFlight int    `json:"principalInFlight"`
	ScopeInFlight     int    `json:"scopeInFlight"`
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
	// time: a gateway's known spend is this alone (ADR 0031). A gateway counts a snapshot of another period as none: a
	// new month starts from nothing.
	Period     string    `json:"period,omitempty"`
	KnownSpend int64     `json:"knownSpend,omitempty"`
	Since      time.Time `json:"since,omitzero"`
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
}

// Price is a model's rates in micro-units per million tokens.
type Price struct {
	Model            string `json:"model"`
	InputPerMTok     int64  `json:"inputPerMTok"`
	OutputPerMTok    int64  `json:"outputPerMTok"`
	CacheReadPerMTok int64  `json:"cacheReadPerMTok,omitempty"`
	// CacheWritePerMTok prices five-minute cache writes, and one-hour
	// writes unless CacheWrite1hPerMTok is set (gateway/price).
	CacheWritePerMTok   int64 `json:"cacheWritePerMTok,omitempty"`
	CacheWrite1hPerMTok int64 `json:"cacheWrite1hPerMTok,omitempty"`
	// Version is the PriceList's, recorded on the rows it prices
	// (price_version): name/version, or name@sha256:<digest> of its spec.
	Version string `json:"version,omitempty"`
}

// Source names the inputs a snapshot was built from (model/freshness.md).
type Source struct {
	StateVersion int64 `json:"stateVersion"`
	// ConfigRevision is the config checkout's git commit (golem-control's
	// -config-link: git-sync's worktree is named by its hash); "" when the
	// config is not read from a checkout.
	ConfigRevision Revision `json:"configRevision"`
	ConfigDigest   string   `json:"configDigest"`
}

// Revision is a config revision, a git commit's hash. Publishers before it
// wrote this member as the number 0, never set; a number reads as "", so
// a gateway upgraded first (operations.md, Upgrade) still reads their
// snapshots.
type Revision string

// UnmarshalJSON reads a string, or an older publisher's number as "".
func (r *Revision) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] != '"' {
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("configRevision: want a commit hash or an older publisher's number: %w", err)
		}
		*r = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*r = Revision(s)
	return nil
}

// Backend is a backend as compiled: its secrets are references, never values
// (ADR 0005).
type Backend struct {
	Name    string `json:"name"`
	Dialect string `json:"dialect"`
	// Envelope is "" or "bedrock" (lib/api's BackendSpec.Envelope).
	Envelope string `json:"envelope,omitempty"`
	// ProviderAccount is the account its calls are billed to (lib/api's
	// BackendSpec.ProviderAccount), the backend's name if that is unset:
	// the ledger's provider_account.
	ProviderAccount string       `json:"providerAccount,omitempty"`
	Auth            Auth         `json:"auth"`
	Deployments     []Deployment `json:"deployments"`
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
