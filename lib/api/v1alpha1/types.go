// Package v1alpha1 holds golem's config CRDs (group golem.dev, ADR 0005) as
// Go types, and reads them from YAML. It has no Kubernetes client
// dependency, so the gateway, golemctl and golem-control share it
// (ADR 0023). This version has Backend, ModelRoute, ScopePolicy,
// PriceList and LocalIdentity; Hook follows.
package v1alpha1

// APIVersion is the apiVersion of every golem config object.
const APIVersion = "golem.dev/v1alpha1"

// Kinds this package reads.
const (
	KindBackend       = "Backend"
	KindModelRoute    = "ModelRoute"
	KindScopePolicy   = "ScopePolicy"
	KindLocalIdentity = "LocalIdentity"
	KindPriceList     = "PriceList"
)

// TypeMeta is an object's apiVersion and kind.
type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// ObjectMeta is the part of Kubernetes' metadata golem reads.
type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	UID         string            `json:"uid,omitempty"`
	Generation  int64             `json:"generation,omitempty"`
}

// Backend is a provider or model server: its dialect, how golem
// authenticates to it, and its deployments (one model at one endpoint
// each).
type Backend struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta  `json:"metadata"`
	Spec     BackendSpec `json:"spec"`
}

// BackendSpec is a Backend's desired state.
type BackendSpec struct {
	// Dialect is the API the backend speaks: anthropic.messages or openai.chat.
	Dialect     string       `json:"dialect"`
	Auth        Auth         `json:"auth"`
	Deployments []Deployment `json:"deployments"`
	Network     Network      `json:"network,omitzero"`
}

// Network grants a backend addresses golem otherwise refuses to dial.
type Network struct {
	// AllowPrivateAddresses lets the backend's deployments be on loopback
	// or private addresses (a self-hosted model server, a developer's local
	// one). Cloud metadata and link-local addresses are refused always.
	AllowPrivateAddresses bool `json:"allowPrivateAddresses,omitempty"`
}

// Auth is how golem authenticates to a backend: a header carrying a secret.
type Auth struct {
	// Header is the header name: x-api-key, authorization, api-key.
	Header string `json:"header"`
	// Scheme prefixes the secret, e.g. "Bearer"; empty for none.
	Scheme string `json:"scheme,omitempty"`
	// SecretRef names a Kubernetes Secret key mounted into the gateway.
	SecretRef *SecretRef `json:"secretRef,omitempty"`
	// Env names an environment variable: standalone mode only (ADR 0023),
	// for a developer's own credentials.
	Env string `json:"env,omitempty"`
}

// SecretRef is a key of a Secret in the gateway's namespace.
type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Deployment is one model at one endpoint of a backend.
type Deployment struct {
	Name string `json:"name"`
	// URL is the endpoint golem posts to, e.g.
	// https://api.anthropic.com/v1/messages.
	URL string `json:"url"`
	// Model is the provider's name for the model.
	Model string `json:"model"`
	// MaxInFlight is this gateway's ceiling on the deployment's calls in
	// flight (its share of the provider's quota, or a local box's static
	// concurrency): an attempt past it is not sent, and the call fails over
	// to another deployment of its pool. 0: none.
	MaxInFlight int `json:"maxInFlight,omitempty"`
}

// ModelRoute maps an alias callers name to a pool of deployments.
type ModelRoute struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta     `json:"metadata"`
	Spec     ModelRouteSpec `json:"spec"`
}

// ModelRouteSpec is a ModelRoute's desired state.
type ModelRouteSpec struct {
	Alias    string         `json:"alias"`
	Pool     []PoolMember   `json:"pool"`
	Timeouts *RouteTimeouts `json:"timeouts,omitempty"`
	// Retries bound the attempts a call makes before it commits
	// (model/attempts.md); the defaults are lib/compile's.
	Retries *RouteRetries `json:"retries,omitempty"`
	// Agent marks a coding-agent route: compatible feature mode by default
	// (ADR 0041 §7.1), under the platform's standing approval.
	Agent bool `json:"agent,omitempty"`
	// ApprovedBetas are the anthropic-beta values restricted mode forwards.
	ApprovedBetas []string `json:"approvedBetas,omitempty"`
}

// RouteTimeouts bound a call's life on a route, as Go durations ("30m").
// Each is optional; the defaults are lib/compile's.
type RouteTimeouts struct {
	// MaxDuration bounds the whole call: past it the provider's response is
	// cut (and its slot freed).
	MaxDuration string `json:"maxDuration,omitempty"`
	// Headers bounds the wait for the response's headers. A streamed answer
	// sends them at once; a non-streamed one when it is finished, so the
	// default is MaxDuration.
	Headers string `json:"headers,omitempty"`
	// Idle bounds a provider's silence once a response has begun: longer
	// than a model's reasoning pause, shorter than a hung connection.
	Idle string `json:"idle,omitempty"`
}

// RouteRetries bound a call's attempts on a route. Only an attempt the
// provider refused or never saw is retried (unreached, 429, 503, 529, or
// an error as the stream's first event); one that may have reached it and
// got no answer ends the call.
type RouteRetries struct {
	// MaxAttempts is the most attempts one call makes, the first included
	// (1: no retries; at most 5).
	MaxAttempts int `json:"maxAttempts,omitempty"`
}

// PoolMember is a deployment in a route's pool.
type PoolMember struct {
	Backend    string `json:"backend"`
	Deployment string `json:"deployment"`
	Weight     int    `json:"weight,omitempty"`
}

// ScopePolicy is a scope's policy (ADR 0005, 0050): what its members may
// call and within which limits. Its name is the scope's ID. A scope exists
// in the state document (created through the control API, which holds its
// memberships, keys and Entra group bindings); its policy is config,
// reviewed in GitOps. In standalone mode, where there is no state, each
// ScopePolicy is a scope a LocalIdentity's keys and members can name.
type ScopePolicy struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta      `json:"metadata"`
	Spec     ScopePolicySpec `json:"spec"`
}

// ScopePolicySpec is a scope's policy. Every field is optional: unset, a
// scope can call nothing and has no budget, the default in-flight limits,
// no ceilings, provider storage forbidden and the route's feature mode.
type ScopePolicySpec struct {
	// Models are the route aliases the scope's members may call.
	Models []string `json:"models,omitempty"`
	// BudgetMicros is the period's budget in micro-units of the currency;
	// zero is no budget.
	BudgetMicros int64 `json:"budgetMicros,omitempty"`
	// BudgetMode is what the budget does at admission (PRD §9.1): observe
	// (the default: calls over it are admitted and their ledger rows
	// marked), warn (admitted, marked, and the response carries a
	// golem-warning header) or enforce (refused, budget.pool_exhausted).
	BudgetMode string `json:"budgetMode,omitempty"`
	// DeclaredWorstCaseCheck admits a call only if known spend plus its declared
	// worst case fits, and refuses calls that declare none (no max_tokens,
	// server-side tools): a fail-closed admission mode, not a spending
	// ceiling (ADR 0044).
	DeclaredWorstCaseCheck bool     `json:"declaredWorstCaseCheck,omitempty"`
	InFlight               InFlight `json:"inFlight,omitzero"`
	// Ceilings on a request (ADR 0038): output tokens asked for (max_tokens
	// times n) and the body's size. Zero is none.
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
	MaxInputBytes   int64 `json:"maxInputBytes,omitempty"`
	// ProviderStorage is "forbidden" (the default: golem turns store off and
	// refuses provider-side state) or "allowed" (ADR 0027).
	ProviderStorage string `json:"providerStorage,omitempty"`
	// Features is the request-feature mode (ADR 0037, 0041 §3): restricted
	// (approved fields, betas and versions only) or compatible (unknown
	// features forwarded); unset, restricted except on agent routes.
	Features string `json:"features,omitempty"`
}

// LocalIdentity is standalone mode's stand-in for the state document's
// entitlements (ADR 0023): dev keys and members of the config's scopes
// (each a ScopePolicy), for testing authentication, authorization and
// budgets on one machine. Refused outside standalone mode.
type LocalIdentity struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta        `json:"metadata"`
	Spec     LocalIdentitySpec `json:"spec"`
}

// LocalIdentitySpec is a LocalIdentity's keys and members. The scopes they
// name are the config's ScopePolicies.
type LocalIdentitySpec struct {
	Keys     []LocalKey    `json:"keys,omitempty"`
	Users    []LocalMember `json:"users,omitempty"`
	Services []LocalMember `json:"services,omitempty"`
}

// LocalMember is a user (by Entra object ID) or a service (by app ID), for
// testing Entra tokens standalone.
type LocalMember struct {
	Name    string   `json:"name"`
	Entra   string   `json:"entra"` // oid, or app (client) ID
	Scopes  []string `json:"scopes"`
	Default string   `json:"default,omitempty"` // default: the first scope
}

// InFlight is a scope's in-flight limits (ADR 0031, 0041 §7.6).
type InFlight struct {
	Principal int `json:"principal,omitempty"` // per principal, default 4
	Scope     int `json:"scope,omitempty"`     // for the scope, default 16
}

// LocalKey is a dev key, given in full (standalone only: production stores
// hashes).
type LocalKey struct {
	Key       string   `json:"key"`
	Principal string   `json:"principal"`
	Scope     string   `json:"scope"`
	Models    []string `json:"models,omitempty"`  // narrows the scope's
	Expires   string   `json:"expires,omitempty"` // RFC 3339
	Revoked   bool     `json:"revoked,omitempty"`
}

// PriceList prices models (contracts.md §4). This first version has the
// per-token rates; tiers, batch discounts and server-side tool units follow.
type PriceList struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta    `json:"metadata"`
	Spec     PriceListSpec `json:"spec"`
}

// PriceListSpec is a currency and its prices.
type PriceListSpec struct {
	Currency string  `json:"currency"`
	Prices   []Price `json:"prices"`
}

// Price is a model's rates in micro-units per million tokens.
type Price struct {
	Model             string `json:"model"` // the provider's model name
	InputPerMTok      int64  `json:"inputPerMTok"`
	OutputPerMTok     int64  `json:"outputPerMTok"`
	CacheReadPerMTok  int64  `json:"cacheReadPerMTok,omitempty"`
	CacheWritePerMTok int64  `json:"cacheWritePerMTok,omitempty"`
}
