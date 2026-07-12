// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package vpnaas implements the pcd_vpnaas_* resources (Neutron VPNaaS v2),
// ported from terraform-provider-openstack v3.4.0.
//
// VPN objects (service, site connection) have an asynchronous delete: the API
// returns immediately and the object lingers in PENDING_DELETE until the backend
// finishes. Resources therefore wait for the object to 404 after delete, so a
// dependent resource (router, subnet) can be torn down in the same apply. Create
// and update are effectively synchronous from the caller's view (the object is
// returned and immediately GETtable), matching the upstream provider, which does
// not wait for ACTIVE.
package vpnaas

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// defaultVPNTimeout bounds each wait for a VPN object to disappear after delete.
const defaultVPNTimeout = 10 * time.Minute

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

// waitForDeletion polls get until it reports a 404, so callers can block until an
// asynchronously-deleted VPN object is fully gone.
func waitForDeletion(ctx context.Context, timeout time.Duration, get func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		err := get(ctx)
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
}

// listToStrings converts a list attribute to a Go slice (nil for null/unknown).
func listToStrings(ctx context.Context, l types.List, diags *diag.Diagnostics) []string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return out
}

// lifetime is the {units, value} nested object shared by the IKE and IPsec
// policy resources.
var lifetimeAttrTypes = map[string]attr.Type{
	"units": types.StringType,
	"value": types.Int64Type,
}

type lifetimeModel struct {
	Units types.String `tfsdk:"units"`
	Value types.Int64  `tfsdk:"value"`
}

// flattenLifetime builds the nested lifetime object from the server's values.
func flattenLifetime(units string, value int) types.Object {
	return types.ObjectValueMust(lifetimeAttrTypes, map[string]attr.Value{
		"units": types.StringValue(units),
		"value": types.Int64Value(int64(value)),
	})
}

// lifetimeFromObject reads the user-supplied lifetime block. ok is false when the
// block is omitted (null/unknown), in which case callers leave the server default
// in place. Missing sub-fields come back as "" / 0 so the caller's omitempty drops
// them and the server fills its own default.
func lifetimeFromObject(ctx context.Context, o types.Object, diags *diag.Diagnostics) (units string, value int, ok bool) {
	if o.IsNull() || o.IsUnknown() {
		return "", 0, false
	}
	var lm lifetimeModel
	diags.Append(o.As(ctx, &lm, basetypes.ObjectAsOptions{UnhandledNullAsEmpty: true, UnhandledUnknownAsEmpty: true})...)
	return lm.Units.ValueString(), int(lm.Value.ValueInt64()), true
}

// dpd is the {action, timeout, interval} nested object on the site connection.
var dpdAttrTypes = map[string]attr.Type{
	"action":   types.StringType,
	"timeout":  types.Int64Type,
	"interval": types.Int64Type,
}

type dpdModel struct {
	Action   types.String `tfsdk:"action"`
	Timeout  types.Int64  `tfsdk:"timeout"`
	Interval types.Int64  `tfsdk:"interval"`
}

// flattenDPD builds the nested dpd object from the server's values.
func flattenDPD(action string, timeout, interval int) types.Object {
	return types.ObjectValueMust(dpdAttrTypes, map[string]attr.Value{
		"action":   types.StringValue(action),
		"timeout":  types.Int64Value(int64(timeout)),
		"interval": types.Int64Value(int64(interval)),
	})
}

// dpdFromObject reads the user-supplied dpd block. ok is false when the block is
// omitted (null/unknown). Missing sub-fields come back as "" / 0 so the caller's
// omitempty drops them and the server fills its own default.
func dpdFromObject(ctx context.Context, o types.Object, diags *diag.Diagnostics) (action string, timeout, interval int, ok bool) {
	if o.IsNull() || o.IsUnknown() {
		return "", 0, 0, false
	}
	var dm dpdModel
	diags.Append(o.As(ctx, &dm, basetypes.ObjectAsOptions{UnhandledNullAsEmpty: true, UnhandledUnknownAsEmpty: true})...)
	return dm.Action.ValueString(), int(dm.Timeout.ValueInt64()), int(dm.Interval.ValueInt64()), true
}
