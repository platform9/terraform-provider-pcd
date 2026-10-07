// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// noPanic runs f and fails the test if f panics. A panic in a resource method
// crashes the provider plugin, which fails the whole apply.
func noPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panicked: %v", p)
		}
	}()
	f()
}

// gophercloud decodes a create answer whose body lacks the object's key to a
// nil object and no error. These creates used to dereference it and crash the
// provider. Each must report an error and leave nothing in state.
func TestCreateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	type create struct {
		name, route string
		newResource func(*clients.Config) resource.Resource
		plan        func(*testing.T, resource.Resource) tfsdk.Plan
	}
	creates := []create{
		{"secgroup", "POST " + secgroupsPath,
			func(c *clients.Config) resource.Resource { return &secgroupResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan {
				return secgroupPlan(t, r.(*secgroupResource), nil, false)
			}},
		{"secgroup_rule", "POST /v2.0/security-group-rules",
			func(c *clients.Config) resource.Resource { return &secgroupRuleResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan {
				return newPlan(t, schemaOf(t, r), &secgroupRuleModel{
					ID: types.StringUnknown(), Direction: types.StringValue("ingress"), EtherType: types.StringValue("IPv4"),
					SecurityGroupID: types.StringValue(secgroupID), Protocol: types.StringValue("tcp"),
					PortRangeMin: types.Int64Value(22), PortRangeMax: types.Int64Value(22),
					RemoteGroupID: types.StringUnknown(), RemoteIPPrefix: types.StringValue("0.0.0.0/0"),
					Description: types.StringNull(), TenantID: types.StringUnknown(), Region: types.StringUnknown(),
				})
			}},
		{"router", "POST " + routersPath,
			func(c *clients.Config) resource.Resource { return &routerResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan { return routerPlan(t, r.(*routerResource), nil) }},
		{"subnet", "POST " + subnetsPath,
			func(c *clients.Config) resource.Resource { return &subnetResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan { return subnetPlan(t, r.(*subnetResource), nil) }},
		{"qos_policy", "POST " + qosPoliciesPath,
			func(c *clients.Config) resource.Resource { return &qosPolicyResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan {
				return qosPolicyPlan(t, r.(*qosPolicyResource), nil)
			}},
	}
	for _, k := range qosRuleKinds() {
		creates = append(creates, create{"qos_" + k.name + "_rule", "POST " + k.rulesPath(), k.newResource,
			func(t *testing.T, r resource.Resource) tfsdk.Plan { return newPlan(t, schemaOf(t, r), k.planned()) }})
	}

	for _, c := range creates {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.newResource(newFakeNeutron(t, neutronRoutes{c.route: reply(http.StatusCreated, `{}`)}).config)
			var resp resource.CreateResponse
			noPanic(t, func() { resp = runCreate(r, c.plan(t, r)) })
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want an error for an answer without the object")
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create left a row: %v", resp.State.Raw)
			}
		})
	}
}
