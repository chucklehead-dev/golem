// Package snapshot is the format of what the gateway runs on: routes and,
// later, entitlements, compiled from the config CRDs and the state
// (ADR 0006), and the signed lease that bounds how long it may be used
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
	"time"
)

// Schema is this format's version; a reader refuses a newer one.
const Schema = 1

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
	ID                string   `json:"id"`
	Models            []string `json:"models"` // aliases its members may call
	BudgetMicros      int64    `json:"budgetMicros,omitempty"`
	Strict            bool     `json:"strict,omitempty"`
	PrincipalInFlight int      `json:"principalInFlight"`
	ScopeInFlight     int      `json:"scopeInFlight"`
	// KnownSpend is the ledger's balance for Period (a calendar month,
	// "2026-11") with this site's events numbered above Watermark left out;
	// Watermark is the site counter's number for the period up to which
	// every event is in the ledger (ADR 0042). A gateway counts a snapshot
	// of another period as none: a new month starts from nothing.
	Period     string `json:"period,omitempty"`
	KnownSpend int64  `json:"knownSpend,omitempty"`
	Watermark  int64  `json:"watermark,omitempty"`
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
	Name        string       `json:"name"`
	Dialect     string       `json:"dialect"`
	Auth        Auth         `json:"auth"`
	Deployments []Deployment `json:"deployments"`
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
}

// Route is an alias and its pool.
type Route struct {
	Alias   string   `json:"alias"`
	Dialect string   `json:"dialect"`
	Pool    []Target `json:"pool"`
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

// Decode reads a snapshot, refusing a schema this reader does not know.
func Decode(b []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Schema != Schema {
		return nil, &SchemaError{Got: s.Schema}
	}
	return &s, nil
}

// SchemaError is a snapshot of a schema this gateway cannot read.
type SchemaError struct{ Got int }

func (e *SchemaError) Error() string {
	return "snapshot schema " + itoa(e.Got) + ", this gateway reads " + itoa(Schema) + ": upgrade the gateway before the publisher"
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Hash is the hex SHA-256 of encoded bytes: a snapshot's name in the bucket
// and in status.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
