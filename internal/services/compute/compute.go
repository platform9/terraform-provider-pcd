// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package compute implements the pcd_compute_* resources and data sources
// (Nova v2), ported from terraform-provider-openstack v3.4.0.
package compute

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// splitInstanceScopedID parses a composite resource/import ID of the form
// "<instance_id>/<sub_id>" (used by the attach resources).
func splitInstanceScopedID(id string) (instanceID, sub string, err error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected import ID in the form <instance_id>/<id>, got %q", id)
	}
	return parts[0], parts[1], nil
}

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

// extractStringMap converts a Terraform map value to a Go map[string]string,
// returning nil for a null/unknown map. Conversion diagnostics are appended.
func extractStringMap(ctx context.Context, m types.Map, diags *diag.Diagnostics) map[string]string {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	out := map[string]string{}
	diags.Append(m.ElementsAs(ctx, &out, false)...)
	return out
}
