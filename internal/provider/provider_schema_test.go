// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// GetProviderSchema validates every resource, data source and action schema
// the provider registers, as Terraform does before any plan. Running it here
// catches an invalid schema (an action attribute marked Computed, say) in CI
// instead of in a user's first plan.
func TestProviderSchemaIsValid(t *testing.T) {
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		t.Errorf("%s: %s: %s", d.Severity, d.Summary, d.Detail)
	}
	if _, ok := resp.ActionSchemas["pcd_compute_instance_reboot"]; !ok {
		names := make([]string, 0, len(resp.ActionSchemas))
		for name := range resp.ActionSchemas {
			names = append(names, name)
		}
		t.Errorf("action pcd_compute_instance_reboot is not served; actions: %v", names)
	}
}
