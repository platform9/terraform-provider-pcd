// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package tfstate holds helpers for resources that write Terraform state
// outside the usual happy path.
package tfstate

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// NullUnknowns replaces every unknown value in state with null. Terraform
// refuses unknown values in state, and a Create that records its object before
// the API has reported the object's computed attributes leaves them unknown.
// The next refresh reads the real values.
func NullUnknowns(state *tfsdk.State) diag.Diagnostics {
	raw, err := tftypes.Transform(state.Raw, func(_ *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if v.IsKnown() {
			return v, nil
		}
		return tftypes.NewValue(v.Type(), nil), nil
	})
	if err != nil {
		return diag.Diagnostics{diag.NewErrorDiagnostic("Preparing Terraform state", err.Error())}
	}
	state.Raw = raw
	return nil
}

// RecordCreated saves a just-created object to state before Create waits for it
// to become usable. The service keeps an object whose build fails or whose wait
// is interrupted, so the wait's error has to be returned with the object in
// state: Terraform then marks the resource tainted, and the next apply or a
// destroy deletes it instead of leaving it behind and creating another. The
// attributes the service has not reported yet are saved as null, since Terraform
// refuses unknown values in state, and the next refresh reads them. It reports
// whether Create should carry on. Callers must set the object's ID on plan
// first: a state row without one names nothing the destroy could delete, so
// RecordCreated drops the row rather than record it.
func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool {
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	resp.Diagnostics.Append(NullUnknowns(&resp.State)...)
	if resp.Diagnostics.HasError() {
		return false
	}
	var id types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return false
	}
	if id.IsNull() || id.ValueString() == "" {
		resp.State.RemoveResource(ctx)
		resp.Diagnostics.AddError("Recording the created object",
			"The object was created but no ID was recorded for it, so it was left out of state. "+
				"This is a bug in the provider.")
		return false
	}
	return true
}
