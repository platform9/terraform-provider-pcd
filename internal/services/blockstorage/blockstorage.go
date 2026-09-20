// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package blockstorage implements the pcd_blockstorage_* resources and data
// sources (Cinder v3), ported from terraform-provider-openstack v3.4.0.
package blockstorage

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

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

// recordCreated saves a just-created object to state before Create waits for it
// to become available. Cinder keeps an object whose build fails (status
// "error"), so a failed or interrupted wait has to return its error with the
// object in state: Terraform then marks the resource tainted, and the next apply
// or a destroy deletes it instead of leaving it behind and creating another. The
// attributes Cinder has not reported yet are saved as null, since Terraform
// refuses unknown values in state, and the next refresh reads them. It reports
// whether Create should carry on.
func recordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool {
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	resp.Diagnostics.Append(tfstate.NullUnknowns(&resp.State)...)
	return !resp.Diagnostics.HasError()
}
