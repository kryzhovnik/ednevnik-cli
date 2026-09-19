package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	jarengine "github.com/kryzhovnik/ednevnik-cli/internal/client/cookiejar"
	"github.com/kryzhovnik/ednevnik-cli/internal/coordination"
)

const DefaultBaseURL = "https://moj.esdnevnik.rs"

const defaultUserAgent = "ednevnik-cli/0.1 (+https://github.com/kryzhovnik/ednevnik-cli)"

var (
	ErrNotAuthenticated        = errors.New("not authenticated; run `ednevnik login`")
	ErrOriginMismatch          = errors.New("request target is outside the configured portal origin")
	ErrAuthRejected            = errors.New("credentials were rejected")
	ErrAuthInteractionRequired = errors.New("interactive authentication is required")
)

type HTTPError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return fmt.Sprintf("eDnevnik returned HTTP %d", e.StatusCode) }

type Client struct {
	base        *url.URL
	http        *http.Client
	lease       *coordination.Lease
	sessionPath string
	account     string
	userAgent   string
}

func New(baseURL, sessionPath, account string, lease *coordination.Lease) (*Client, error) {
	return newClientWithMode(baseURL, sessionPath, account, lease, http.DefaultTransport, false)
}

func newClient(baseURL, sessionPath, account string, lease *coordination.Lease, roundTripper http.RoundTripper) (*Client, error) {
	return newClientWithMode(baseURL, sessionPath, account, lease, roundTripper, false)
}

func NewFresh(baseURL, sessionPath, account string, lease *coordination.Lease) (*Client, error) {
	return newClientWithMode(baseURL, sessionPath, account, lease, http.DefaultTransport, true)
}

func newClientWithMode(baseURL, sessionPath, account string, lease *coordination.Lease, roundTripper http.RoundTripper, fresh bool) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if base.User != nil || base.Hostname() == "" || base.Path != "" || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && !(base.Scheme == "http" && isLoopbackHost(base.Hostname()))) {
		return nil, errors.New("client base URL must be an exact HTTPS origin")
	}
	if account == "" {
		return nil, errors.New("client requires an explicit account")
	}
	var jar *jarengine.Jar
	if fresh {
		jar, err = newJar()
	} else {
		jar, err = loadJar(sessionPath, base, account)
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if lease == nil {
		return nil, errors.New("client requires an account coordination lease")
	}
	transport := &policyTransport{base: roundTripper, lease: lease}
	c := &Client{
		base:  base,
		http:  &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: transport},
		lease: lease, sessionPath: sessionPath,
		account:   account,
		userAgent: defaultUserAgent,
	}
	c.http.CheckRedirect = c.checkRedirect
	return c, nil
}

// SetUserAgentVersion identifies the exact CLI build to the portal.
func (c *Client) SetUserAgentVersion(version string) {
	if version != "" {
		c.userAgent = fmt.Sprintf("ednevnik-cli/%s (+https://github.com/kryzhovnik/ednevnik-cli)", version)
	}
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func (c *Client) Login(ctx context.Context, username, password string) error {
	return c.LoginWithCommit(ctx, username, password, nil)
}

// LoginWithCommit verifies the selected credentials, runs commit before the
// new cookie snapshot becomes durable, and then saves the session.
func (c *Client) LoginWithCommit(ctx context.Context, username, password string, commit func() error) error {
	jar, err := newJar()
	if err != nil {
		return err
	}
	// Explicit authentication never inherits a prior account's cookies.
	c.http.Jar = jar
	body, finalURL, err := c.get(ctx, "/login", false)
	if err != nil {
		return err
	}
	if finalURL.Path != "/login" {
		return fmt.Errorf("%w: login endpoint did not present the expected credential form", ErrAuthInteractionRequired)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	token, ok := doc.Find(`input[name="_token"]`).First().Attr("value")
	if !ok || token == "" {
		return fmt.Errorf("%w: login form has no CSRF token", ErrAuthInteractionRequired)
	}
	form := url.Values{"_token": {token}, "username": {username}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.ResolveReference(&url.URL{Path: "/login"}).String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := readResponseBody(resp.Body); err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return &HTTPError{StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &HTTPError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Request.URL.Path == "/login" {
		return ErrAuthRejected
	}
	verified, verifiedURL, err := c.get(ctx, "/", true)
	if err != nil {
		return fmt.Errorf("verify authenticated session: %w", err)
	}
	if verifiedURL.Path == "/login" || !authenticatedPage(verified) {
		return ErrAuthInteractionRequired
	}
	if commit != nil {
		if err := commit(); err != nil {
			return err
		}
	}
	return saveJar(c.sessionPath, jar, c.base, c.account)
}

func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	body, finalURL, err := c.get(ctx, path, true)
	jar, ok := c.http.Jar.(*jarengine.Jar)
	if !ok {
		return nil, errors.New("unsupported session jar")
	}
	if err := saveJar(c.sessionPath, jar, c.base, c.account); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}
	if err != nil {
		return nil, err
	}
	if finalURL.Path == "/login" {
		return nil, ErrNotAuthenticated
	}
	return body, nil
}

func (c *Client) get(ctx context.Context, path string, authenticated bool) ([]byte, *url.URL, error) {
	target, err := c.resolveTarget(path)
	if err != nil {
		return nil, nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			delays := []time.Duration{5 * time.Second, 20 * time.Second}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(delays[attempt-1]):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		resp, err := c.do(req)
		if err != nil {
			return nil, nil, err
		}
		body, readErr := readResponseBody(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, resp.Request.URL, &HTTPError{StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, resp.Request.URL, &HTTPError{StatusCode: resp.StatusCode}
		}
		if resp.StatusCode >= 500 {
			lastErr = &HTTPError{StatusCode: resp.StatusCode}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return nil, resp.Request.URL, &HTTPError{StatusCode: resp.StatusCode}
		}
		if authenticated && resp.Request.URL.Path == "/login" {
			return nil, resp.Request.URL, ErrNotAuthenticated
		}
		return body, resp.Request.URL, nil
	}
	return nil, nil, lastErr
}

func (c *Client) resolveTarget(raw string) (*url.URL, error) {
	target, err := c.base.Parse(raw)
	if err != nil {
		return nil, err
	}
	if target.User != nil || target.Scheme != c.base.Scheme || target.Host != c.base.Host {
		return nil, ErrOriginMismatch
	}
	return target, nil
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if _, err := c.resolveTarget(req.URL.String()); err != nil {
		return err
	}
	previous := via[len(via)-1]
	if previous.Method != http.MethodGet && previous.Method != http.MethodHead && req.Method == previous.Method {
		return errors.New("refusing redirect that repeats a credential-bearing request")
	}
	return nil
}

func authenticatedPage(body []byte) bool {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return false
	}
	if doc.Find(`form input[name="password"], form[action*="/login"] input[name="username"]`).Length() != 0 {
		return false
	}
	return doc.Find(`.students-list, .card.student, timeline, a[href*="logout"]`).Length() != 0
}

func (c *Client) ResetSession() error {
	jar, err := newJar()
	if err != nil {
		return err
	}
	c.http.Jar = jar
	return resetSession(c.sessionPath)
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	return c.http.Do(req)
}

func (c *Client) BudgetStatus() (string, int, int, error) {
	count, limit, _, err := c.lease.Status()
	return time.Now().UTC().Format("2006-01-02"), count, limit, err
}

type policyTransport struct {
	base  http.RoundTripper
	lease *coordination.Lease
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.lease.BeforeRequest(req.Context()); err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if resp != nil {
		if policyErr := t.lease.RecordResponse(resp); policyErr != nil {
			if resp.Body != nil {
				resp.Body.Close()
			}
			return nil, policyErr
		}
	}
	return resp, err
}

func retryAfter(raw string) time.Duration {
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil && when.After(time.Now()) {
		return time.Until(when)
	}
	return 6 * time.Hour
}
