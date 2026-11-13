// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// keystoneDefaultDomainID is the ID Keystone gives its default domain
// ([identity] default_domain_id), the domain the PCD admin user lives in.
const keystoneDefaultDomainID = "default"

// tokenScope is who the provider is authenticated as: the user behind the
// token it holds and the domains that user and its scope belong to.
type tokenScope struct {
	UserID          string
	UserDomainID    string
	ProjectDomainID string
	DomainID        string
}

// currentTokenScope reads the provider's own token back from Keystone, the
// same call the pcd_identity_auth_scope data source makes. A part of the
// token that does not decode is an error, never an absent scope, so the
// guards that use it fail closed.
func currentTokenScope(ctx context.Context, client *gophercloud.ServiceClient) (tokenScope, error) {
	result := tokens.Get(ctx, client, client.Token())
	if result.Err != nil {
		return tokenScope{}, fmt.Errorf("reading the provider's token: %w", result.Err)
	}
	var s tokenScope
	user, err := clients.RequireObject(result.ExtractUser())
	if err != nil {
		return tokenScope{}, fmt.Errorf("reading the provider's token: %w", err)
	}
	s.UserID = user.ID
	s.UserDomainID = user.Domain.ID
	project, err := result.ExtractProject()
	if err != nil {
		return tokenScope{}, fmt.Errorf("reading the provider's token: %w", err)
	}
	if project != nil {
		s.ProjectDomainID = project.Domain.ID
	}
	domain, err := result.ExtractDomain()
	if err != nil {
		return tokenScope{}, fmt.Errorf("reading the provider's token: %w", err)
	}
	if domain != nil {
		s.DomainID = domain.ID
	}
	return s, nil
}

// protectedDomainReason says why the provider must not disable or delete the
// domain with this ID, or returns "" when it may. Disabling the domain the
// provider's user or project belongs to revokes the provider's own token in
// the middle of the apply, and deleting it also deletes that user or project.
func (s tokenScope) protectedDomainReason(id string) string {
	if id == "" {
		return ""
	}
	if id == keystoneDefaultDomainID {
		return "it is Keystone's default domain, which holds the PCD administrator"
	}
	return s.ownDomainReason(id)
}

// ownDomainReason says why the provider's own token depends on the domain with
// this ID, or returns "" when it does not. Unlike protectedDomainReason it does
// not treat Keystone's default domain specially.
func (s tokenScope) ownDomainReason(id string) string {
	if id == "" {
		return ""
	}
	switch id {
	case s.UserDomainID:
		return "the user the provider authenticates as belongs to it"
	case s.ProjectDomainID:
		return "the project the provider is scoped to belongs to it"
	case s.DomainID:
		return "the provider's token is scoped to it"
	}
	return ""
}
