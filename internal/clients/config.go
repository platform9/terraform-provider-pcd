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
//
// PCD runs two Glance deployments sharing one database: a control-plane pod
// behind the public endpoint whose default store is pod-local `file`, and the
// image-library host's Glance behind the ADMIN endpoint, backed by the storage
// the cluster blueprint declares. An image uploaded through the public endpoint
// lands in the pod's local store, where no hypervisor can fetch it — the boot
// then fails with "Image not found in any configured backend". The PCD UI
// uploads via the admin endpoint for exactly this reason, so this client
// prefers it too, falling back to the public endpoint only when no admin
// endpoint is in the catalog. Deployments where the admin endpoint is not
// routable from where Terraform runs should set `endpoint_overrides.image` to
// a reachable URL for the image-library host's Glance.
func (c *Config) ImageV2Client() (*gophercloud.ServiceClient, error) {
	opts := c.endpointOpts()
	opts.Availability = gophercloud.AvailabilityAdmin
	client, err := openstack.NewImageV2(c.Provider, opts)
	if err != nil {
		client, err = openstack.NewImageV2(c.Provider, c.endpointOpts())
		if err != nil {
			return nil, fmt.Errorf("pcd: creating image v2 client: %w", err)
		}
	}
	c.applyOverride(client, "image")
	return client, nil
}

// NetworkV2Client returns a Neutron v2 service client, honoring an
// endpoint_overrides entry for the "network" service type if present.
func (c *Config) NetworkV2Client() (*gophercloud.ServiceClient, error) {
	client, err := openstack.NewNetworkV2(c.Provider, c.endpointOpts())
	if err != nil {
		return nil, fmt.Errorf("pcd: creating network v2 client: %w", err)
	}
	c.applyOverride(client, "network")
	return client, nil
}

// ComputeV2Client returns a Nova v2 service client, honoring an endpoint_overrides
// entry for the "compute" service type if present.
func (c *Config) ComputeV2Client() (*gophercloud.ServiceClient, error) {
	client, err := openstack.NewComputeV2(c.Provider, c.endpointOpts())
	if err != nil {
		return nil, fmt.Errorf("pcd: creating compute v2 client: %w", err)
	}
	c.applyOverride(client, "compute")
	return client, nil
}

// BlockStorageV3Client returns a Cinder v3 service client, honoring an
// endpoint_overrides entry for the "volumev3" service type if present.
func (c *Config) BlockStorageV3Client() (*gophercloud.ServiceClient, error) {
	client, err := openstack.NewBlockStorageV3(c.Provider, c.endpointOpts())
	if err != nil {
		return nil, fmt.Errorf("pcd: creating block storage v3 client: %w", err)
	}
	c.applyOverride(client, "volumev3")
	return client, nil
}

// resmgrClient returns a client for a specific PCD resource manager (`resmgr`)
// API version. resmgr is a Platform9-specific REST service (not part of
// OpenStack, so it has no gophercloud constructor).
//
// The two versions are NOT interchangeable and the provider needs both:
//
//	v2  cluster blueprints, host configs, host-config assignment
//	    (v1 serves none of these — /v1/blueprint and /v1/hostconfigs are 404)
//	v1  host role assignment
//	    (v2 has no writable roles sub-resource — PUT/DELETE
//	    /v2/hosts/<id>/roles/<name> returns 404 RoleNotFound — and its read
//	    view reports mapped "uber-roles" such as `hypervisor` instead of the
//	    granular `pf9-*` names roles are assigned by)
//
// The base URL is resolved from the Keystone catalog (service type `resmgr`)
// and the shared authenticated ProviderClient supplies the token, so requests
// use `client.Get/Post/Put/Delete`.
func (c *Config) resmgrClient(version string) (*gophercloud.ServiceClient, error) {
	url, err := c.Provider.EndpointLocator(gophercloud.EndpointOpts{
		Type:         "resmgr",
		Region:       c.Region,
		Availability: gophercloud.AvailabilityPublic,
	})
	if err != nil {
		return nil, fmt.Errorf("pcd: locating resmgr endpoint: %w", err)
	}
	// An endpoint_overrides entry for "resmgr" names the service, not one of
	// its API versions, so the version this client needs is (re-)applied to it.
	// That lets a single override serve both versions; applyOverride cannot be
	// used here because it replaces the endpoint wholesale, which would send
	// v1 calls to a v2 URL.
	base := url
	if override, ok := c.EndpointOverrides["resmgr"]; ok && override != "" {
		base = override
	}
	return &gophercloud.ServiceClient{
		ProviderClient: c.Provider,
		Endpoint:       resmgrVersionedURL(base, version),
		Type:           "resmgr",
	}, nil
}

// resmgrVersionedURL returns base carrying exactly one trailing API-version
// segment. Any version already on base is replaced, so a bare service root and
// an already-versioned URL (which an endpoint_overrides entry may supply) both
// resolve correctly.
func resmgrVersionedURL(base, version string) string {
	b := strings.TrimRight(base, "/")
	if i := strings.LastIndex(b, "/"); i != -1 {
		switch b[i+1:] {
		case "v1", "v2":
			b = b[:i]
		}
	}
	return b + "/" + version + "/"
}

// ResmgrV2Client returns a resmgr v2 client: cluster blueprints, host configs,
// and host-config assignment.
func (c *Config) ResmgrV2Client() (*gophercloud.ServiceClient, error) {
	return c.resmgrClient("v2")
}

// ResmgrV1Client returns a resmgr v1 client. Host role assignment lives only
// here; see resmgrClient for why v2 cannot serve it.
func (c *Config) ResmgrV1Client() (*gophercloud.ServiceClient, error) {
	return c.resmgrClient("v1")
}

// LoadBalancerV2Client returns an Octavia v2 service client, honoring an
// endpoint_overrides entry for the "load-balancer" service type if present.
func (c *Config) LoadBalancerV2Client() (*gophercloud.ServiceClient, error) {
	client, err := openstack.NewLoadBalancerV2(c.Provider, c.endpointOpts())
	if err != nil {
		return nil, fmt.Errorf("pcd: creating load balancer v2 client: %w", err)
	}
	c.applyOverride(client, "load-balancer")
	return client, nil
}

// DNSV2Client returns a Designate v2 service client, honoring an
// endpoint_overrides entry for the "dns" service type if present.
func (c *Config) DNSV2Client() (*gophercloud.ServiceClient, error) {
	client, err := openstack.NewDNSV2(c.Provider, c.endpointOpts())
	if err != nil {
		return nil, fmt.Errorf("pcd: creating dns v2 client: %w", err)
	}
	c.applyOverride(client, "dns")
	return client, nil
}

// KeyManagerV1Client returns a Barbican (key manager) v1 service client, honoring
// an endpoint_overrides entry for the "key-manager" service type if present.
func (c *Config) KeyManagerV1Client() (*gophercloud.ServiceClient, error) {
	client, err := openstack.NewKeyManagerV1(c.Provider, c.endpointOpts())
	if err != nil {
		return nil, fmt.Errorf("pcd: creating key manager v1 client: %w", err)
	}
	c.applyOverride(client, "key-manager")
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
