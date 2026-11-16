package entrasync

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Graph is the directory as Microsoft Graph serves it: a group's
// transitive user members, paged, and a user's account, live or deleted.
// It needs the application permissions GroupMember.Read.All and
// User.Read.All (entra.md #4: accountEnabled and the deleted items are not
// basic properties).
type Graph struct {
	Token  func(ctx context.Context) (string, error)
	Base   string // default https://graph.microsoft.com/v1.0
	Client *http.Client
	// MaxPages bounds one group's pages (default 1000, a million users at
	// Graph's 999 a page): a server that pages for ever is an error.
	MaxPages int
}

// graphUser is a user as Graph serves it ($select=id,userPrincipalName,accountEnabled).
type graphUser struct {
	ID             string `json:"id"`
	UPN            string `json:"userPrincipalName"`
	AccountEnabled *bool  `json:"accountEnabled"`
}

// user is u as a User. An account whose state Graph does not report is
// neither enabled nor disabled: a member of nothing, and its removal not
// taken for offboarding.
func (u graphUser) user() User {
	e := u.AccountEnabled
	return User{ID: u.ID, UPN: u.UPN, Enabled: e != nil && *e, Disabled: e != nil && !*e}
}

func (g *Graph) base() string {
	return strings.TrimRight(cmp.Or(g.Base, "https://graph.microsoft.com/v1.0"), "/")
}

// Members lists group's users, nested groups' included.
func (g *Graph) Members(ctx context.Context, group string) ([]User, error) {
	base := g.base()
	next := base + "/groups/" + url.PathEscape(group) +
		"/transitiveMembers/microsoft.graph.user?$select=id,userPrincipalName,accountEnabled&$top=999"
	var users []User
	for range cmp.Or(g.MaxPages, 1000) {
		var page struct {
			Value []graphUser `json:"value"`
			Next  string      `json:"@odata.nextLink"`
		}
		if err := g.get(ctx, next, &page); err != nil {
			if errors.Is(err, errNotFound) {
				return nil, ErrNoGroup
			}
			return nil, err
		}
		for _, u := range page.Value {
			users = append(users, u.user())
		}
		if page.Next == "" {
			return users, nil
		}
		// The next page must be Graph's: a link elsewhere would carry the
		// token there.
		if !strings.HasPrefix(page.Next, base+"/") {
			return nil, fmt.Errorf("group %s: a next page outside %s", group, base)
		}
		next = page.Next
	}
	return nil, fmt.Errorf("group %s: more than %d pages", group, cmp.Or(g.MaxPages, 1000))
}

// User looks up the account with object ID id: the live user, or failing
// that the deleted one (Deleted); ErrNoUser if Graph has neither (purged,
// or not this tenant's). A deletion is taken only from the deleted items,
// never from a 404 alone, so a lookup failing for every user cannot
// offboard everyone past the removal guard.
func (g *Graph) User(ctx context.Context, id string) (User, error) {
	base := g.base()
	var u graphUser
	err := g.get(ctx, base+"/users/"+url.PathEscape(id)+"?$select=id,userPrincipalName,accountEnabled", &u)
	if err == nil {
		return u.user(), nil
	}
	if !errors.Is(err, errNotFound) {
		return User{}, err
	}
	switch err := g.get(ctx, base+"/directory/deletedItems/"+url.PathEscape(id)+"?$select=id,userPrincipalName", &u); {
	case errors.Is(err, errNotFound):
		return User{}, ErrNoUser
	case err != nil:
		return User{}, err
	}
	return User{ID: u.ID, UPN: u.UPN, Deleted: true}, nil
}

// errNotFound is Graph's 404: no such group, user or deleted item.
var errNotFound = errors.New("Graph: 404 Not Found")

func (g *Graph) get(ctx context.Context, u string, into any) error {
	tok, err := g.Token(ctx)
	if err != nil {
		return fmt.Errorf("a Graph token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	c := g.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode != http.StatusOK:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Graph: %s: %s", resp.Status, b)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(into)
}

// FederatedToken is an app-only token for Graph by workload identity
// federation (entra.md: no client secret): the cluster's projected service
// account token, from TokenFile, is the client assertion. Tokens are cached
// until a minute before they expire.
type FederatedToken struct {
	Tenant, ClientID string
	TokenFile        string // default $AZURE_FEDERATED_TOKEN_FILE
	Authority        string // default https://login.microsoftonline.com
	Scope            string // default https://graph.microsoft.com/.default
	Client           *http.Client

	mu      sync.Mutex
	tok     string
	expires time.Time
}

// Token is a current token.
func (f *FederatedToken) Token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tok != "" && time.Until(f.expires) > time.Minute {
		return f.tok, nil
	}
	assertion, err := os.ReadFile(cmp.Or(f.TokenFile, os.Getenv("AZURE_FEDERATED_TOKEN_FILE")))
	if err != nil {
		return "", fmt.Errorf("the federated token file: %w", err)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {f.ClientID},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {strings.TrimSpace(string(assertion))},
		"scope":                 {cmp.Or(f.Scope, "https://graph.microsoft.com/.default")}}
	endpoint := strings.TrimRight(cmp.Or(f.Authority, "https://login.microsoftonline.com"), "/") + "/" + url.PathEscape(f.Tenant) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c := f.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return "", fmt.Errorf("Entra's token endpoint: %s: %w", resp.Status, err)
	}
	if tr.Error != "" || tr.AccessToken == "" {
		return "", fmt.Errorf("Entra refused the federated credential: %s: %s", cmp.Or(tr.Error, resp.Status), tr.Description)
	}
	f.tok, f.expires = tr.AccessToken, time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second)
	return f.tok, nil
}
