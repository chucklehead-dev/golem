// Package v1alpha1 holds golem's config CRDs (group golem.dev, ADR 0005) as
// Go types, and reads them from YAML. It has no Kubernetes client
// dependency, so the gateway, golemctl and golem-control share it
// (ADR 0023). This first version has Backend and ModelRoute; ScopePolicy,
// Hook and PriceList follow.
package v1alpha1

// APIVersion is the apiVersion of every golem config object.
const APIVersion = "golem.dev/v1alpha1"

// Kinds this package reads.
const (
	KindBackend    = "Backend"
	KindModelRoute = "ModelRoute"
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
}

// ModelRoute maps an alias callers name to a pool of deployments.
type ModelRoute struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta     `json:"metadata"`
	Spec     ModelRouteSpec `json:"spec"`
}

// ModelRouteSpec is a ModelRoute's desired state.
type ModelRouteSpec struct {
	Alias string       `json:"alias"`
	Pool  []PoolMember `json:"pool"`
}

// PoolMember is a deployment in a route's pool.
type PoolMember struct {
	Backend    string `json:"backend"`
	Deployment string `json:"deployment"`
	Weight     int    `json:"weight,omitempty"`
}
