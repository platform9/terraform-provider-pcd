// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package keymanager implements the pcd_keymanager_* resources and data sources
// (Barbican key manager v1), ported from terraform-provider-openstack v3.4.0.
//
// Barbican identifies objects by a full URL "ref" (secret_ref/container_ref, e.g.
// https://host/v1/secrets/<uuid>), while the API's Get/Delete calls take the bare
// UUID. Resources store the UUID as their ID and expose the full ref as a computed
// attribute; refToID extracts the UUID from a ref.
package keymanager

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"
	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// Barbican secret status values (plain strings; no exported constants).
const (
	secretActive = "ACTIVE"
	secretError  = "ERROR"
)

// defaultKeyManagerTimeout bounds a wait for a secret to become active.
const defaultKeyManagerTimeout = 5 * time.Minute

// configureClient extracts the shared *clients.Config from ProviderData.
func configureClient(providerData any, diags *diag.Diagnostics) *clients.Config {
	if providerData == nil {
		return nil
	}
	config, ok := providerData.(*clients.Config)
	if !ok {
		diags.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *clients.Config, got %T. This is a bug in the provider.", providerData),
		)
		return nil
	}
	return config
}

// refToID returns the trailing UUID of a Barbican ref URL. It is a no-op for a
// value that is already a bare UUID.
func refToID(ref string) string {
	return path.Base(strings.TrimRight(ref, "/"))
}

// waitForSecretActive blocks until a secret reaches ACTIVE, failing on
// ERROR/timeout. Used after creating a secret with a payload (which is briefly
// PENDING). A secret created without a payload stays PENDING, so callers must
// only wait when a payload was supplied.
func waitForSecretActive(ctx context.Context, client *gophercloud.ServiceClient, uuid string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		s, err := secrets.Get(ctx, client, uuid).Extract()
		if err != nil {
			return false, err
		}
		switch s.Status {
		case secretActive:
			return true, nil
		case secretError:
			return false, fmt.Errorf("secret %s entered ERROR status", uuid)
		default:
			return false, nil
		}
	})
	if err != nil {
		return fmt.Errorf("waiting for secret %s to become active: %w", uuid, err)
	}
	return nil
}
