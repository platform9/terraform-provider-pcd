// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CloudConfig holds the auth-relevant fields resolved from a single clouds.yaml
// entry. These values are the lowest-precedence defaults: explicit provider
// configuration and OS_* environment variables override them.
type CloudConfig struct {
	AuthURL           string
	Region            string
	Username          string
	UserID            string
	Password          string
	TenantName        string
	TenantID          string
	UserDomainID      string
	UserDomainName    string
	ProjectDomainID   string
	ProjectDomainName string
	Token             string
	AppCredID         string
	AppCredName       string
	AppCredSecret     string
	CACertFile        string

	// Insecure mirrors clouds.yaml `verify` (verify: false => insecure: true).
	// HasInsecure records whether `verify` was present at all, so callers can
	// distinguish "not set" from "explicitly true".
	Insecure    bool
	HasInsecure bool
}

// cloudsFile is the subset of clouds.yaml this provider understands.
type cloudsFile struct {
	Clouds map[string]cloudEntry `yaml:"clouds"`
}

type cloudEntry struct {
	Auth       cloudAuth `yaml:"auth"`
	RegionName string    `yaml:"region_name"`
	Verify     *bool     `yaml:"verify"`
	CACert     string    `yaml:"cacert"`
}

type cloudAuth struct {
	AuthURL                     string `yaml:"auth_url"`
	Username                    string `yaml:"username"`
	UserID                      string `yaml:"user_id"`
	Password                    string `yaml:"password"`
	ProjectName                 string `yaml:"project_name"`
	ProjectID                   string `yaml:"project_id"`
	UserDomainName              string `yaml:"user_domain_name"`
	UserDomainID                string `yaml:"user_domain_id"`
	ProjectDomainName           string `yaml:"project_domain_name"`
	ProjectDomainID             string `yaml:"project_domain_id"`
	DomainName                  string `yaml:"domain_name"`
	DomainID                    string `yaml:"domain_id"`
	Token                       string `yaml:"token"`
	ApplicationCredentialID     string `yaml:"application_credential_id"`
	ApplicationCredentialName   string `yaml:"application_credential_name"`
	ApplicationCredentialSecret string `yaml:"application_credential_secret"`
}

// LoadCloud finds a clouds.yaml file and returns the named cloud's resolved
// configuration. It searches, in order: $OS_CLIENT_CONFIG_FILE, ./clouds.yaml,
// ~/.config/openstack/clouds.yaml, and /etc/openstack/clouds.yaml.
//
// Only the subset of clouds.yaml relevant to authentication is read. Secrets
// split into a separate secure.yaml, cloud "profiles", and clouds-public.yaml
// are not yet resolved.
func LoadCloud(name string) (*CloudConfig, error) {
	path, data, err := findCloudsYAML()
	if err != nil {
		return nil, err
	}

	var cf cloudsFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("pcd: parsing %s: %w", path, err)
	}

	entry, ok := cf.Clouds[name]
	if !ok {
		return nil, fmt.Errorf("pcd: cloud %q not found in %s", name, path)
	}

	// A single `domain_name`/`domain_id` under auth applies to both the user and
	// project domain unless a more specific key overrides it.
	cc := &CloudConfig{
		AuthURL:           entry.Auth.AuthURL,
		Region:            entry.RegionName,
		Username:          entry.Auth.Username,
		UserID:            entry.Auth.UserID,
		Password:          entry.Auth.Password,
		TenantName:        entry.Auth.ProjectName,
		TenantID:          entry.Auth.ProjectID,
		UserDomainName:    firstNonEmpty(entry.Auth.UserDomainName, entry.Auth.DomainName),
		UserDomainID:      firstNonEmpty(entry.Auth.UserDomainID, entry.Auth.DomainID),
		ProjectDomainName: firstNonEmpty(entry.Auth.ProjectDomainName, entry.Auth.DomainName),
		ProjectDomainID:   firstNonEmpty(entry.Auth.ProjectDomainID, entry.Auth.DomainID),
		Token:             entry.Auth.Token,
		AppCredID:         entry.Auth.ApplicationCredentialID,
		AppCredName:       entry.Auth.ApplicationCredentialName,
		AppCredSecret:     entry.Auth.ApplicationCredentialSecret,
		CACertFile:        entry.CACert,
	}
	if entry.Verify != nil {
		cc.HasInsecure = true
		cc.Insecure = !*entry.Verify
	}
	return cc, nil
}

func findCloudsYAML() (string, []byte, error) {
	var candidates []string
	if p := os.Getenv("OS_CLIENT_CONFIG_FILE"); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "clouds.yaml")
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "openstack", "clouds.yaml"))
	}
	candidates = append(candidates, filepath.Join("/etc", "openstack", "clouds.yaml"))

	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			return p, data, nil
		}
	}
	return "", nil, fmt.Errorf(
		"pcd: no clouds.yaml found (searched OS_CLIENT_CONFIG_FILE, ./clouds.yaml, ~/.config/openstack/clouds.yaml, /etc/openstack/clouds.yaml)")
}
