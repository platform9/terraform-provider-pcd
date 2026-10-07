// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// secgroupModelFor is sg-1 as state holds it after a create whose config set
// name, description and stateful.
func secgroupModelFor(t *testing.T, stateful bool) secgroupModel {
	t.Helper()
	tags, d := types.SetValueFrom(context.Background(), types.StringType, []string{"workload"})
	if d.HasError() {
		t.Fatalf("building the tags: %v", d)
	}
	return secgroupModel{
		ID:                 types.StringValue(secgroupID),
		Name:               types.StringValue(secgroupName),
		Description:        types.StringValue(secgroupDesc),
		Stateful:           types.BoolValue(stateful),
		DeleteDefaultRules: types.BoolValue(false),
		TenantID:           types.StringValue("proj-1"),
		Tags:               tags,
		Region:             types.StringValue("region-one"),
	}
}

// statefulSecgroupRoutes answers for sg-1 the way Neutron does, keeping the
// group's stateful flag in *stateful: a PUT that carries stateful changes it,
// and a GET reports it.
func statefulSecgroupRoutes(t *testing.T, stateful *bool) neutronRoutes {
	t.Helper()
	get := func(w http.ResponseWriter, r *http.Request) {
		body := strings.Replace(secgroupJSON, `"stateful": true`, `"stateful": `+strconv.FormatBool(*stateful), 1)
		reply(http.StatusOK, `{"security_group": `+body+`}`)(w, r)
	}
	routes := secgroupRoutes()
	routes["GET "+secgroupPath] = get
	routes["PUT "+secgroupPath] = func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SecurityGroup map[string]any `json:"security_group"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding the update request: %v", err)
		}
		if v, ok := req.SecurityGroup["stateful"].(bool); ok {
			*stateful = v
		}
		get(w, r)
	}
	return routes
}

// An update that changes stateful used to send only name and description, so
// Neutron kept the old flag, and the read-back returned it against a plan that
// held the new one: Terraform failed the apply with "Provider produced
// inconsistent result after apply". Update must send the new flag.
func TestSecgroupUpdateSendsStateful(t *testing.T) {
	t.Parallel()
	stateful := true
	r := &secgroupResource{config: newFakeNeutron(t, statefulSecgroupRoutes(t, &stateful)).config}
	s := schemaOf(t, r)

	prior := secgroupModelFor(t, true)
	planned := prior
	planned.Stateful = types.BoolValue(false)

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if stateful {
		t.Fatal("update did not send stateful = false; Neutron still has the group stateful")
	}
	if got := secgroupRow(t, resp.State); !got.Stateful.Equal(planned.Stateful) {
		t.Fatalf("update state stateful = %s, want the planned %s: Terraform reports an inconsistent result",
			got.Stateful, planned.Stateful)
	}
}

// delete_default_rules acts only when the group is created, and is documented
// to force a new resource. A change used to plan an in-place update that sent
// nothing for it, so a group whose config turned it on kept Neutron's default
// egress rules while state said they were deleted.
func TestSecgroupDeleteDefaultRulesChangeForcesReplacement(t *testing.T) {
	t.Parallel()
	r := &secgroupResource{}
	for _, tc := range []struct{ prior, planned bool }{{false, true}, {true, false}} {
		prior := secgroupModelFor(t, true)
		prior.DeleteDefaultRules = types.BoolValue(tc.prior)
		planned := prior
		planned.DeleteDefaultRules = types.BoolValue(tc.planned)

		if _, replace := planUpdate(t, r, "delete_default_rules", &prior, &planned); !replace {
			t.Errorf("changing delete_default_rules from %t to %t plans an in-place update; want a replacement",
				tc.prior, tc.planned)
		}
	}
}

// An import leaves delete_default_rules null, since Neutron does not report it,
// and a config that leaves it unset plans its default of false. That first plan
// must not replace the imported group.
func TestSecgroupImportedDeleteDefaultRulesDoesNotForceReplacement(t *testing.T) {
	t.Parallel()
	r := &secgroupResource{}
	prior := secgroupModelFor(t, true)
	prior.DeleteDefaultRules = types.BoolNull()
	planned := prior
	planned.DeleteDefaultRules = types.BoolValue(false)

	if _, replace := planUpdate(t, r, "delete_default_rules", &prior, &planned); replace {
		t.Fatal("the first plan after an import replaces the security group")
	}
}
