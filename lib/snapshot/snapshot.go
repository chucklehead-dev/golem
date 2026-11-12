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
