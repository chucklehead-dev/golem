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
type settings struct {
	Tenant    string `json:"tenant"`
	ClientID  string `json:"clientId"`
	API       string `json:"api"`
	Audience  string `json:"audience"`
	Authority string `json:"authority,omitempty"` // default https://login.microsoftonline.com
	Token     tokens `json:"token"`
}

type tokens struct {
	Access  string    `json:"access"`
	Refresh string    `json:"refresh"`
	Expires time.Time `json:"expires"`
}

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

func (s settings) endpoint(path string) string {
	return strings.TrimRight(cmp.Or(s.Authority, "https://login.microsoftonline.com"), "/") + "/" + url.PathEscape(s.Tenant) + "/oauth2/v2.0/" + path
}

func (s settings) scope() string {
	return strings.TrimRight(s.Audience, "/") + "/golem.access offline_access"
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
	audience := fs.String("audience", cmp.Or(prev.Audience, "api://golem"), "golem API's Application ID URI")
	authority := fs.String("authority", prev.Authority, "the Entra authority (default https://login.microsoftonline.com)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	s := settings{Tenant: *tenant, ClientID: *client, API: *api, Audience: *audience, Authority: *authority}
	if s.Tenant == "" || s.ClientID == "" || s.API == "" {
		return usageError("login needs --tenant, --client-id and --api (they are remembered)")
	}
	var dc struct {
		DeviceCode string `json:"device_code"`
		Message    string `json:"message"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
		Error      string `json:"error"`
		Desc       string `json:"error_description"`
	}
	if err := post(ctx, s.endpoint("devicecode"), url.Values{"client_id": {s.ClientID}, "scope": {s.scope()}}, &dc); err != nil {
		return fmt.Errorf("Entra's device code: %w", err)
	}
	if dc.Error != "" {
		return fmt.Errorf("Entra: %s: %s", dc.Error, dc.Desc)
	}
	fmt.Fprintln(errOut, dc.Message)
	interval := time.Duration(cmp.Or(dc.Interval, 5)) * pollUnit
	deadline := time.Now().Add(time.Duration(cmp.Or(dc.ExpiresIn, 900)) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		var tr tokenResponse
		if err := post(ctx, s.endpoint("token"), url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id": {s.ClientID}, "device_code": {dc.DeviceCode}}, &tr); err != nil {
			return fmt.Errorf("Entra's token endpoint: %w", err)
		}
		switch tr.Error {
		case "":
			s.Token = tokens{Access: tr.AccessToken, Refresh: tr.RefreshToken, Expires: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)}
			if err := s.save(); err != nil {
				return err
			}
			fmt.Fprintln(errOut, "signed in")
			return nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * pollUnit
		default:
			return fmt.Errorf("sign-in failed: %s: %s", tr.Error, tr.Description)
		}
		if time.Now().After(deadline) {
			return errors.New("the device code expired before sign-in: run golemctl login again")
		}
	}
}

// accessToken is a current access token for golem: the cached one while it
// has a minute left, else a refreshed one (saved).
func accessToken(ctx context.Context) (string, error) {
	s, err := loadSettings()
	if err != nil {
		return "", err
	}
	if s.Token.Access != "" && time.Until(s.Token.Expires) > time.Minute {
		return s.Token.Access, nil
	}
	if s.Token.Refresh == "" {
		return "", errors.New("signed out: golemctl login")
	}
	var tr tokenResponse
	if err := post(ctx, s.endpoint("token"), url.Values{"grant_type": {"refresh_token"}, "client_id": {s.ClientID},
		"refresh_token": {s.Token.Refresh}, "scope": {s.scope()}}, &tr); err != nil {
		return "", fmt.Errorf("refreshing the token: %w", err)
	}
	if tr.Error != "" {
		return "", fmt.Errorf("the session has ended (%s): golemctl login", tr.Error)
	}
	s.Token = tokens{Access: tr.AccessToken, Refresh: cmp.Or(tr.RefreshToken, s.Token.Refresh),
		Expires: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)}
	return s.Token.Access, s.save()
}
