// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package clients holds the shared, authenticated OpenStack/PCD client wiring
// that every resource and data source hangs off of via the provider's
// ProviderData. gophercloud is the canonical Go OpenStack SDK; PCD-specific
// services (resmgr, hamgr, mors) are added as thin REST clients in subpackages.
package clients

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
)

// Config is the resolved provider configuration plus the authenticated
// gophercloud ProviderClient. It is constructed once in the provider's
// Configure step and shared (read-only after Authenticate) with every
// resource and data source. No global state: everything hangs off this value.
type Config struct {
	AuthURL string
	Region  string

	// Identity (user) credentials.
	Username string
	UserID   string
	Password string
	Passcode string // TOTP

	// Project scope.
	TenantID   string
	TenantName string

	// Domains: user domain vs. project (scope) domain are distinct in Keystone v3.
	UserDomainID      string
	UserDomainName    string
	ProjectDomainID   string
	ProjectDomainName string

	// Pre-issued token auth.
	Token string

	// Application credential auth.
	AppCredID     string
	AppCredName   string
	AppCredSecret string

	// TLS. CE uses a self-signed cert, so Insecure/CACertFile are first-class.
	Insecure       bool
	CACertFile     string
	ClientCertFile string
	ClientKeyFile  string

	// Behavior knobs (ported incrementally from terraform-provider-openstack).
	MaxRetries  int
	AllowReauth bool

	// EndpointOverrides maps a Keystone service type -> URL, an escape hatch for
	// labs where the catalog endpoint is not reachable as advertised.
	EndpointOverrides map[string]string

	// Provider is the authenticated client, populated by Authenticate.
	Provider *gophercloud.ProviderClient
}

// authOptions builds gophercloud auth options from the resolved config. Password,
// token, and application-credential flows are all supported; scope is set for
// everything except application-credential auth (which is inherently scoped).
func (c *Config) authOptions() gophercloud.AuthOptions {
	ao := gophercloud.AuthOptions{
		IdentityEndpoint:            c.AuthURL,
		Username:                    c.Username,
		UserID:                      c.UserID,
		Password:                    c.Password,
		Passcode:                    c.Passcode,
		TokenID:                     c.Token,
		ApplicationCredentialID:     c.AppCredID,
		ApplicationCredentialName:   c.AppCredName,
		ApplicationCredentialSecret: c.AppCredSecret,
		AllowReauth:                 c.AllowReauth,
		// DomainID/DomainName here identify the *user's* domain for password auth.
		DomainID:   c.UserDomainID,
		DomainName: c.UserDomainName,
	}

	// Application-credential auth must not carry a scope.
	if c.AppCredID == "" && c.AppCredName == "" && (c.TenantID != "" || c.TenantName != "") {
		ao.Scope = &gophercloud.AuthScope{
			ProjectID:   c.TenantID,
			ProjectName: c.TenantName,
			DomainID:    firstNonEmpty(c.ProjectDomainID, c.UserDomainID),
			DomainName:  firstNonEmpty(c.ProjectDomainName, c.UserDomainName),
		}
	}

	return ao
}

// Authenticate builds the TLS-configured ProviderClient and authenticates it.
// Idempotent: a second call with Provider already set is a no-op.
func (c *Config) Authenticate(ctx context.Context) error {
	if c.Provider != nil {
		return nil
	}

	client, err := openstack.NewClient(c.AuthURL)
	if err != nil {
		return fmt.Errorf("pcd: creating client for %s: %w", c.AuthURL, err)
	}

	tlsConfig, err := c.tlsConfig()
	if err != nil {
		return err
	}
	client.HTTPClient = http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
			Proxy:           http.ProxyFromEnvironment,
		},
	}

	// Never let the auth request body (which carries the password) leak into an
	// error: gophercloud's auth errors surface the response, not the request.
	if err := openstack.Authenticate(ctx, client, c.authOptions()); err != nil {
		return fmt.Errorf("pcd: authenticating to %s: %w", c.AuthURL, err)
	}

	c.Provider = client
	return nil
}

// tlsConfig assembles a *tls.Config honoring insecure, cacert_file, and client
// cert/key. Verification is only skipped when the user explicitly sets insecure.
func (c *Config) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if c.Insecure {
		cfg.InsecureSkipVerify = true
	}

	if c.CACertFile != "" {
		pem, err := os.ReadFile(c.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("pcd: reading cacert_file %s: %w", c.CACertFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("pcd: no certificates could be parsed from cacert_file %s", c.CACertFile)
		}
		cfg.RootCAs = pool
	}

	if c.ClientCertFile != "" || c.ClientKeyFile != "" {
		if c.ClientCertFile == "" || c.ClientKeyFile == "" {
			return nil, fmt.Errorf("pcd: cert and key must be provided together")
		}
		cert, err := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("pcd: loading client cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// endpointOpts returns the common endpoint selection options. Region may be
// overridden per resource/data source in a later phase.
func (c *Config) endpointOpts() gophercloud.EndpointOpts {
	return gophercloud.EndpointOpts{
		Region:       c.Region,
		Availability: gophercloud.AvailabilityPublic,
	}
}

// IdentityV3Client returns a Keystone v3 service client, honoring an
// endpoint_overrides entry for the "identity" service type if present.
func (c *Config) IdentityV3Client() (*gophercloud.ServiceClient, error) {
	client, err := openstack.NewIdentityV3(c.Provider, c.endpointOpts())
	if err != nil {
		return nil, fmt.Errorf("pcd: creating identity v3 client: %w", err)
	}
	c.applyOverride(client, "identity")
	return client, nil
}

// ImageV2Client returns a Glance v2 service client, honoring an endpoint_overrides
// entry for the "image" service type if present.
func (c *Config) ImageV2Client() (*gophercloud.ServiceClient, error) {
	client, err := openstack.NewImageV2(c.Provider, c.endpointOpts())
	if err != nil {
		return nil, fmt.Errorf("pcd: creating image v2 client: %w", err)
	}
	c.applyOverride(client, "image")
	return client, nil
}

// applyOverride points a service client at an operator-supplied endpoint when
// endpoint_overrides names its service type.
func (c *Config) applyOverride(client *gophercloud.ServiceClient, serviceType string) {
	if url, ok := c.EndpointOverrides[serviceType]; ok && url != "" {
		client.Endpoint = normalizeEndpoint(url)
		client.ResourceBase = ""
	}
}

func normalizeEndpoint(url string) string {
	if !strings.HasSuffix(url, "/") {
		return url + "/"
	}
	return url
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
