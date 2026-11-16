package proxy

import (
	"net/http"
	"slices"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// ServeModels serves GET /v1/models and GET /v1/models/{id}: the aliases
// the caller may call, in the dialect it calls with (Anthropic's shape if
// it sends anthropic-version, as the Anthropic SDKs and agents do;
// OpenAI's otherwise, Chat's and Responses' routes), the routes its
// endpoints serve: another dialect's would be refused there
// (route.wrong_frontend). A model the caller may not use is not listed,
// and asked for by ID is unknown: listing it would tell a caller what
// other scopes have. Nothing is admitted or charged.
func (h *Handler) ServeModels(w http.ResponseWriter, r *http.Request) {
	d := OpenAIChat
	if r.Header.Get("Anthropic-Version") != "" {
		d = AnthropicMessages
	}
	_, reqIDs := newRequestID()
	w.Header()[contract.HeaderRequestID] = []string{reqIDs}
	if err := contract.CheckSingleHeaders(r.Header); err != nil {
		refuse(w, d, contract.RequestInvalid, err.Error(), 0)
		return
	}
	var grant auth.Grant
	snap, p, no := h.authenticate(r, &grant)
	if no != nil {
		refuse(w, d, no.code, no.msg, no.retryAfter)
		return
	}
	var ids []string
	dialects := []string{d.Name()}
	if d == OpenAIChat { // an OpenAI caller's list: its Chat and Responses routes
		dialects = append(dialects, OpenAIResponses.Name())
	}
	for _, name := range dialects {
		for _, alias := range snap.Routes.Aliases(name) {
			if p.Allows([]byte(alias)) {
				ids = append(ids, alias)
			}
		}
	}
	slices.Sort(ids)
	w.Header()["Content-Type"] = jsonType
	if id := r.PathValue("id"); id != "" {
		if !slices.Contains(ids, id) {
			refuse(w, d, contract.RouteUnknownModel, "no model "+id+" for this key's scope; GET /v1/models lists those it may use", 0)
			return
		}
		w.Write(appendModel(nil, d, id))
		return
	}
	w.Write(appendModels(nil, d, ids))
}

// appendModel is one model object in a dialect's shape. golem's aliases
// have no creation date: the epoch stands for it.
func appendModel(b []byte, d Dialect, id string) []byte {
	if d == AnthropicMessages {
		b = append(b, `{"type":"model","id":`...)
		b = appendQuoted(b, id)
		b = append(b, `,"display_name":`...)
		b = appendQuoted(b, id)
		return append(b, `,"created_at":"1970-01-01T00:00:00Z"}`...)
	}
	b = append(b, `{"id":`...)
	b = appendQuoted(b, id)
	return append(b, `,"object":"model","created":0,"owned_by":"golem"}`...)
}

// appendModels is a list of models in a dialect's shape, whole (no pages).
func appendModels(b []byte, d Dialect, ids []string) []byte {
	if d == AnthropicMessages {
		b = append(b, `{"data":[`...)
	} else {
		b = append(b, `{"object":"list","data":[`...)
	}
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendModel(b, d, id)
	}
	b = append(b, ']')
	if d == AnthropicMessages {
		b = append(b, `,"has_more":false,"first_id":`...)
		first, last := "", ""
		if len(ids) > 0 {
			first, last = ids[0], ids[len(ids)-1]
		}
		b = appendQuotedOrNull(b, first)
		b = append(b, `,"last_id":`...)
		b = appendQuotedOrNull(b, last)
	}
	return append(b, '}')
}

func appendQuotedOrNull(b []byte, s string) []byte {
	if s == "" {
		return append(b, "null"...)
	}
	return appendQuoted(b, s)
}
