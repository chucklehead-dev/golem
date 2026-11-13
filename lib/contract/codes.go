// Package contract holds the interfaces other systems depend on, as code:
// refusal codes with their statuses (contracts.md §1), error origins (§2)
// and golem's header names (§5). A test reads contracts.md and fails when
// the two disagree, so the document stays the source and the code cannot
// drift from it.
package contract

// Origin is who caused a failure (contracts.md §2).
type Origin string

const (
	OriginClient   Origin = "client"
	OriginPolicy   Origin = "policy"
	OriginProvider Origin = "provider"
	OriginGateway  Origin = "gateway"
)

// Code is a refusal: its stable name, HTTP status and origin. Every refusal
// the gateway sends uses one of the values below, so none can be sent
// undocumented.
type Code struct {
	Name   string
	Status int
	Origin Origin
}

// RetryAfter reports whether the status is one the SDKs retry, and so is sent
// with Retry-After (docs/spikes/sdk-compat.md).
func (c Code) RetryAfter() bool { return c.Status == 429 || c.Status == 503 }

// The codes, as contracts.md §1 lists them.
var (
	LimitRate          = Code{"limit.rate", 429, OriginPolicy}
	LimitConcurrency   = Code{"limit.concurrency", 429, OriginPolicy}
	LimitProviderQuota = Code{"limit.provider_quota", 429, OriginPolicy}

	BudgetPoolExhausted = Code{"budget.pool_exhausted", 402, OriginPolicy}
	BudgetMemberCap     = Code{"budget.member_cap", 402, OriginPolicy}
	BudgetKeyCap        = Code{"budget.key_cap", 402, OriginPolicy}

	AuthMissing              = Code{"auth.missing", 401, OriginClient}
	AuthKeyUnknown           = Code{"auth.key_unknown", 401, OriginClient}
	AuthKeyExpired           = Code{"auth.key_expired", 401, OriginClient}
	AuthKeyRevoked           = Code{"auth.key_revoked", 401, OriginClient}
	AuthKeyWrongInstallation = Code{"auth.key_wrong_installation", 401, OriginClient}
	AuthTokenInvalid         = Code{"auth.token_invalid", 401, OriginClient}
	AuthTokenExpired         = Code{"auth.token_expired", 401, OriginClient}

	AuthzModelNotAllowed          = Code{"authz.model_not_allowed", 403, OriginPolicy}
	AuthzScopeNotMember           = Code{"authz.scope_not_member", 403, OriginPolicy}
	AuthzProviderStorageForbidden = Code{"authz.provider_storage_forbidden", 403, OriginPolicy}
	AuthzOriginForbidden          = Code{"authz.origin_forbidden", 403, OriginClient}

	RequestInvalid    = Code{"request.invalid", 400, OriginClient}
	RequestTooLarge   = Code{"request.too_large", 413, OriginClient}
	RequestUnreadable = Code{"request.unreadable", 400, OriginClient}

	RouteUnknownModel         = Code{"route.unknown_model", 404, OriginClient}
	RouteNoCapableDeployment  = Code{"route.no_capable_deployment", 400, OriginClient}
	RouteWrongFrontend        = Code{"route.wrong_frontend", 400, OriginClient}
	TranslateUnsupportedField = Code{"translate.unsupported_feature", 400, OriginClient}

	UpstreamUnavailable  = Code{"upstream.unavailable", 503, OriginProvider}
	UpstreamStreamFailed = Code{"upstream.stream_failed", 0, OriginProvider} // an error event in the stream

	GatewayAuthorizationExpired = Code{"gateway.authorization_expired", 503, OriginGateway}
	GatewayNotReady             = Code{"gateway.not_ready", 503, OriginGateway}
	GatewayInternal             = Code{"gateway.internal", 500, OriginGateway}
)

// Codes is every code (read-only).
var Codes = []Code{
	LimitRate, LimitConcurrency, LimitProviderQuota,
	BudgetPoolExhausted, BudgetMemberCap, BudgetKeyCap,
	AuthMissing, AuthKeyUnknown, AuthKeyExpired, AuthKeyRevoked, AuthKeyWrongInstallation, AuthTokenInvalid, AuthTokenExpired,
	AuthzModelNotAllowed, AuthzScopeNotMember, AuthzProviderStorageForbidden, AuthzOriginForbidden,
	RequestInvalid, RequestTooLarge, RequestUnreadable,
	RouteUnknownModel, RouteNoCapableDeployment, RouteWrongFrontend, TranslateUnsupportedField,
	UpstreamUnavailable, UpstreamStreamFailed,
	GatewayAuthorizationExpired, GatewayNotReady, GatewayInternal,
}

// Headers golem reads or sets (contracts.md §5). golem never forwards its
// own headers upstream.
const (
	HeaderRequestID  = "Golem-Request-Id"
	HeaderWarning    = "Golem-Warning"
	HeaderRoute      = "Golem-Route"
	HeaderTranslated = "Golem-Translated"
	HeaderSession    = "Golem-Session"
	HeaderScope      = "Golem-Scope"
	HeaderTags       = "Golem-Tags"
	HeaderEndUser    = "Golem-End-User"
)

// Headers is every golem header (read-only).
var Headers = []string{HeaderRequestID, HeaderWarning, HeaderRoute, HeaderTranslated,
	HeaderSession, HeaderScope, HeaderTags, HeaderEndUser}
