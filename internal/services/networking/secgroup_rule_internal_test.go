// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Neutron cannot change a security group rule, so Update sends nothing. A
// change to port_range_min or port_range_max used to plan an in-place update:
// state took the new port while the rule kept the old one, and every later
// plan showed the change again. A port change must replace the rule.
func TestSecgroupRulePortRangeChangeForcesReplacement(t *testing.T) {
	t.Parallel()
	r := &secgroupRuleResource{}
	prior := secgroupRuleModel{
		ID:              types.StringValue("rule-ssh"),
		Direction:       types.StringValue("ingress"),
		EtherType:       types.StringValue("IPv4"),
		SecurityGroupID: types.StringValue(secgroupID),
		Protocol:        types.StringValue("tcp"),
		PortRangeMin:    types.Int64Value(22),
		PortRangeMax:    types.Int64Value(22),
		RemoteGroupID:   types.StringValue(""),
		RemoteIPPrefix:  types.StringValue("0.0.0.0/0"),
		Description:     types.StringNull(),
		TenantID:        types.StringValue("proj-1"),
		Region:          types.StringValue("region-one"),
	}
	planned := prior
	planned.PortRangeMin = types.Int64Value(2222)
	planned.PortRangeMax = types.Int64Value(2222)

	for _, name := range []string{"port_range_min", "port_range_max"} {
		if _, replace := planUpdate(t, r, name, &prior, &planned); !replace {
			t.Errorf("changing %s from 22 to 2222 plans an in-place update, which sends nothing; want a replacement", name)
		}
	}
}
