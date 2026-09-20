// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package tfstate holds helpers for resources that write Terraform state
// outside the usual happy path.
package tfstate

import (
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
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
