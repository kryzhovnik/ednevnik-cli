package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/kryzhovnik/ednevnik/internal/coordination"
)

const DefaultBaseURL = "https://moj.esdnevnik.rs"

var ErrNotAuthenticated = errors.New("not authenticated; run `ednevnik login`")

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
	userAgent   string
}

func New(baseURL, sessionPath string, lease *coordination.Lease) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	jar, err := loadJar(sessionPath, base)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if lease == nil {
		return nil, errors.New("client requires an account coordination lease")
	}
	transport := &policyTransport{base: http.DefaultTransport, lease: lease}
	return &Client{
		base:  base,
		http:  &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: transport},
		lease: lease, sessionPath: sessionPath,
		userAgent: "ednevnik-cli/0.1 (+https://github.com/kryzhovnik/ednevnik)",
	}, nil
}

func (c *Client) Login(ctx context.Context, username, password string) error {
	body, finalURL, err := c.get(ctx, "/login", false)
	if err != nil {
		return err
	}
	if finalURL.Path != "/login" {
		return saveJar(c.sessionPath, c.http.Jar, c.base)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	token, ok := doc.Find(`input[name="_token"]`).First().Attr("value")
	if !ok || token == "" {
		return errors.New("login form has no CSRF token; the site may have changed")
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
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.Request.URL.Path == "/login" {
		return errors.New("login failed; check the credentials and complete any required browser verification")
	}
	return saveJar(c.sessionPath, c.http.Jar, c.base)
}

func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	body, finalURL, err := c.get(ctx, path, true)
	if err != nil {
		return nil, err
	}
	if finalURL.Path == "/login" {
		return nil, ErrNotAuthenticated
	}
	if err := saveJar(c.sessionPath, c.http.Jar, c.base); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}
	return body, nil
}

func (c *Client) get(ctx context.Context, path string, authenticated bool) ([]byte, *url.URL, error) {
	target, err := c.base.Parse(path)
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
