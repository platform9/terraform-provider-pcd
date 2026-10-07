// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// snatRouterRoutes answers for router-1 the way Neutron does, keeping SNAT on
// its gateway in *snat. A PUT that carries external_gateway_info sets SNAT to
// its enable_snat or, when it has none, to Neutron's default of true
// (enable_snat_by_default).
func snatRouterRoutes(t *testing.T, snat *bool) neutronRoutes {
	t.Helper()
	get := func(w http.ResponseWriter, r *http.Request) {
		body := strings.Replace(routerJSON, `"enable_snat": true`, `"enable_snat": `+strconv.FormatBool(*snat), 1)
		reply(http.StatusOK, `{"router": `+body+`}`)(w, r)
	}
	routes := routerRoutes()
	routes["GET "+routerPath] = get
	routes["PUT "+routerPath] = func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Router struct {
				GatewayInfo *struct {
					EnableSNAT *bool `json:"enable_snat"`
				} `json:"external_gateway_info"`
			} `json:"router"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding the update request: %v", err)
		}
		if gw := req.Router.GatewayInfo; gw != nil {
			*snat = gw.EnableSNAT == nil || *gw.EnableSNAT
		}
		get(w, r)
	}
	return routes
}

// A router whose gateway has SNAT off, for example one imported that way, and
// whose config leaves enable_snat unset plans it unknown on any update. Update
// used to resend the gateway then, with network_id alone, and Neutron set SNAT
// back to its default: a rename turned SNAT on. The update must leave the
// gateway alone.
func TestRouterUpdateKeepsSNATItDoesNotManage(t *testing.T) {
	t.Parallel()
	snat := false
	r := &routerResource{config: newFakeNeutron(t, snatRouterRoutes(t, &snat)).config}
	s := schemaOf(t, r)

	prior := routerPriorState(t)
	prior.EnableSNAT = types.BoolValue(false)
	planned := routerRenamePlan(prior)
	enableSNAT, _ := planUpdate(t, r, "enable_snat", &prior, &planned)
	planned.EnableSNAT = enableSNAT.(types.Bool)

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if snat {
		t.Fatal("a rename turned SNAT on: Update resent the gateway without enable_snat")
	}
	if got := routerRow(t, resp.State); !got.EnableSNAT.Equal(types.BoolValue(false)) {
		t.Fatalf("update state enable_snat = %s, want false", got.EnableSNAT)
	}
}
