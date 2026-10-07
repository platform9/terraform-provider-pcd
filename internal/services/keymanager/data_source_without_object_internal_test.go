// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package keymanager

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// gophercloud decodes a Barbican answer as the object itself, so a 200 whose
// body is JSON null decodes to a nil secret and no error, and one whose body
// lacks the secret decodes to a secret with every field empty, without the
// secret_ref Barbican always sends. The secret data source's lookup by
// secret_ref used to crash on the first and save empty values for the second.
// It must report an error for both and set nothing.
func TestDataSourceReadRefusesAnAnswerWithoutTheSecret(t *testing.T) {
	t.Parallel()
	answers := []struct{ name, body, want string }{
		{"JSON null", `null`, clients.ErrNoObject.Error()},
		{"empty object", `{}`, "without a secret object"},
	}
	for _, a := range answers {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := &secretDataSource{config: newFakeBarbican(t, barbicanRoutes{"GET " + secretPath: reply(http.StatusOK, a.body)}).config}
			var schemaResp datasource.SchemaResponse
			d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
			s := schemaResp.Schema
			typ := s.Type().TerraformType(ctx).(tftypes.Object)
			vals := make(map[string]tftypes.Value, len(typ.AttributeTypes))
			for name, at := range typ.AttributeTypes {
				vals[name] = tftypes.NewValue(at, nil)
			}
			vals["secret_ref"] = tftypes.NewValue(tftypes.String, "http://barbican.invalid/v1/secrets/sec-1")
			config := tfsdk.Config{Schema: s, Raw: tftypes.NewValue(typ, vals)}
			resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(typ, nil)}}
			noPanic(t, func() { d.Read(ctx, datasource.ReadRequest{Config: config}, &resp) })
			reported := false
			for _, e := range resp.Diagnostics.Errors() {
				reported = reported || strings.Contains(e.Detail(), a.want)
			}
			if !reported {
				t.Fatalf("read diagnostics = %v; want an error for the answer without the object", resp.Diagnostics)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("read set %v; want nothing set", resp.State.Raw)
			}
		})
	}
}
