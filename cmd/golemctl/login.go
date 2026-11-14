package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// settings are where golem is and the cached tokens, in one file only its
// owner reads (GOLEMCTL_HOME, else the user's config directory). The
// design keeps tokens in the OS keychain (tools.md §1); a file is this
// slice's.
//
// Two resources take golemctl's tokens (ADR 0046): the control API's
// registration (Control, scope golem.manage), which the commands call, and
// golem API (Gateway, scope golem.access), whose tokens golemctl token
// prints for coding agents. One sign-in's refresh token redeems both; each
// access token is cached for its own resource, so a gateway token handed
// to an agent cannot call the control API.
type settings struct {
	Tenant    string           `json:"tenant"`
	ClientID  string           `json:"clientId"`
	API       string           `json:"api"`
	Control   string           `json:"control"`             // the control API's Application ID URI
	Gateway   string           `json:"gateway"`             // golem API's Application ID URI
	Authority string           `json:"authority,omitempty"` // default https://login.microsoftonline.com
	Refresh   string           `json:"refresh"`
	Tokens    map[string]token `json:"tokens,omitempty"` // by resource: "control", "gateway"
}

// token is a cached access token for one resource.
type token struct {
	Access  string    `json:"access"`
	Expires time.Time `json:"expires"`
}

// A resource golemctl gets tokens for.
type resource string

const (
	control resource = "control"
	gateway resource = "gateway"
)

// settingsPath is the settings file's path.
func settingsPath() (string, error) {
	if h := os.Getenv("GOLEMCTL_HOME"); h != "" {
		return filepath.Join(h, "golemctl.json"), nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "golem", "golemctl.json"), nil
}

// loadSettings reads the settings, or says how to sign in if there are none.
func loadSettings() (settings, error) {
	var s settings
	p, err := settingsPath()
	if err != nil {
		return s, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return s, errors.New("not signed in: golemctl login --tenant <id> --client-id <id> --api <url>")
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// save writes the settings, owner-only, by a rename so that the file is
// never half-written.
func (s settings) save() error {
	p, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// endpoint is the URL of the tenant's OAuth endpoint path ("token").
func (s settings) endpoint(path string) string {
	return strings.TrimRight(cmp.Or(s.Authority, "https://login.microsoftonline.com"), "/") + "/" + url.PathEscape(s.Tenant) + "/oauth2/v2.0/" + path
}

// scope is what a token for r asks for, with a refresh token.
func (s settings) scope(r resource) string {
	if r == gateway {
		return strings.TrimRight(s.Gateway, "/") + "/golem.access offline_access"
	}
	return strings.TrimRight(s.Control, "/") + "/golem.manage offline_access"
}

var httpClient = &http.Client{Timeout: 30 * time.Second} // read-only

// pollUnit is Entra's interval unit, a second (tests shorten it).
var pollUnit = time.Second

// tokenResponse is the token endpoint's answer, or its error.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// post sends form to endpoint and decodes its JSON answer into into.
func post(ctx context.Context, endpoint string, form url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

// login signs in by Entra's device code flow (entra.md #2: golemctl is a
// public client) and saves the settings and tokens.
func login(ctx context.Context, args []string, _, errOut io.Writer) error {
	prev, _ := loadSettings()
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(errOut)
	tenant := fs.String("tenant", prev.Tenant, "the Entra tenant (directory) ID")
	client := fs.String("client-id", prev.ClientID, "golemctl's app registration's client ID (entra.md #2)")
	api := fs.String("api", prev.API, "golem-control's URL")
	ctl := fs.String("control", cmp.Or(prev.Control, "api://golem-control"), "the control API registration's Application ID URI")
	gw := fs.String("gateway", cmp.Or(prev.Gateway, "api://golem"), "golem API's (the gateway's) Application ID URI")
	authority := fs.String("authority", prev.Authority, "the Entra authority (default https://login.microsoftonline.com)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	s := settings{Tenant: *tenant, ClientID: *client, API: *api, Control: *ctl, Gateway: *gw, Authority: *authority}
	if s.Tenant == "" || s.ClientID == "" || s.API == "" {
		return usageError("login needs --tenant, --client-id and --api (they are remembered)")
	}
	var dc deviceCode
	if err := post(ctx, s.endpoint("devicecode"), url.Values{"client_id": {s.ClientID}, "scope": {s.scope(control)}}, &dc); err != nil {
		return fmt.Errorf("Entra's device code: %w", err)
	}
	if dc.Error != "" {
		return fmt.Errorf("Entra: %s: %s", dc.Error, dc.Desc)
	}
	fmt.Fprintln(errOut, dc.Message)
	tr, err := awaitSignIn(ctx, s, dc)
	if err != nil {
		return err
	}
	// The sign-in asked for the control API's scope, so its access token
	// is the control API's; the gateway's is redeemed when first needed.
	s.Refresh = tr.RefreshToken
	s.Tokens = map[string]token{string(control): {Access: tr.AccessToken, Expires: expiry(tr)}}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintln(errOut, "signed in")
	return nil
}

// deviceCode is the device code endpoint's answer, or its error.
type deviceCode struct {
	DeviceCode string `json:"device_code"`
	Message    string `json:"message"` // what the user is to do, for them to read
	Interval   int    `json:"interval"`
	ExpiresIn  int    `json:"expires_in"`
	Error      string `json:"error"`
	Desc       string `json:"error_description"`
}

// awaitSignIn polls the token endpoint for dc at Entra's interval (slower
// when it says slow_down) until the user signs in, which yields both an
// access and a refresh token, or the code expires.
func awaitSignIn(ctx context.Context, s settings, dc deviceCode) (tokenResponse, error) {
	interval := time.Duration(cmp.Or(dc.Interval, 5)) * pollUnit
	deadline := time.Now().Add(time.Duration(cmp.Or(dc.ExpiresIn, 900)) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return tokenResponse{}, ctx.Err()
		case <-time.After(interval):
		}
		var tr tokenResponse
		if err := post(ctx, s.endpoint("token"), url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id": {s.ClientID}, "device_code": {dc.DeviceCode}}, &tr); err != nil {
			return tokenResponse{}, fmt.Errorf("Entra's token endpoint: %w", err)
		}
		switch tr.Error {
		case "":
			if tr.AccessToken == "" || tr.RefreshToken == "" {
				return tokenResponse{}, errors.New("Entra's answer had no access or refresh token: run golemctl login again")
			}
			return tr, nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * pollUnit
		default:
			return tokenResponse{}, fmt.Errorf("sign-in failed: %s: %s", tr.Error, tr.Description)
		}
		if time.Now().After(deadline) {
			return tokenResponse{}, errors.New("the device code expired before sign-in: run golemctl login again")
		}
	}
}

// expiry is when tr's access token expires.
func expiry(tr tokenResponse) time.Time {
	return time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
}

// accessToken is a current access token for r: the cached one while it has
// a minute left, else one redeemed with the refresh token (saved).
func accessToken(ctx context.Context, r resource) (string, error) {
	s, err := loadSettings()
	if err != nil {
		return "", err
	}
	if t := s.Tokens[string(r)]; t.Access != "" && time.Until(t.Expires) > time.Minute {
		return t.Access, nil
	}
	if s.Refresh == "" {
		return "", errors.New("signed out: golemctl login")
	}
	var tr tokenResponse
	if err := post(ctx, s.endpoint("token"), url.Values{"grant_type": {"refresh_token"}, "client_id": {s.ClientID},
		"refresh_token": {s.Refresh}, "scope": {s.scope(r)}}, &tr); err != nil {
		return "", fmt.Errorf("refreshing the token: %w", err)
	}
	if tr.Error != "" {
		return "", fmt.Errorf("the session has ended (%s): golemctl login", tr.Error)
	}
	if tr.AccessToken == "" {
		return "", errors.New("Entra's answer had no access token: golemctl login")
	}
	if s.Tokens == nil {
		s.Tokens = map[string]token{}
	}
	s.Refresh = cmp.Or(tr.RefreshToken, s.Refresh)
	s.Tokens[string(r)] = token{Access: tr.AccessToken, Expires: expiry(tr)}
	return tr.AccessToken, s.save()
}
