// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// fipAssocNewPortID is the port an update moves fip-1 to, and
// fipAssocNewPortFixedIP its only address.
const (
	fipAssocNewPortID      = "port-2"
	fipAssocNewPortFixedIP = "10.0.1.7"
)

// movingFIPAssocRoutes answers for fip-1 the way Neutron does, with fip-1
// associated with port-1 at 10.0.0.5 until a PUT moves it. Neutron answers a
// PUT whose fixed_ip_address is not an address of its port_id with 400, and
// maps a PUT without one to the port's first address.
func movingFIPAssocRoutes(t *testing.T) neutronRoutes {
	t.Helper()
	portIPs := map[string]string{fipAssocPortID: fipAssocFixedIP, fipAssocNewPortID: fipAssocNewPortFixedIP}
	portID, fixedIP := fipAssocPortID, fipAssocFixedIP
	get := func(w http.ResponseWriter, r *http.Request) {
		reply(http.StatusOK, `{"floatingip": `+fipAssocJSON(portID, fixedIP)+`}`)(w, r)
	}
	return neutronRoutes{
		"GET " + floatingIPPath: get,
		"PUT " + floatingIPPath: func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				FloatingIP struct {
					PortID  string `json:"port_id"`
					FixedIP string `json:"fixed_ip_address"`
				} `json:"floatingip"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decoding the update request: %v", err)
			}
			want := portIPs[req.FloatingIP.PortID]
			if req.FloatingIP.FixedIP != "" && req.FloatingIP.FixedIP != want {
				reply(http.StatusBadRequest, `{"NeutronError": {"type": "BadRequest", "message": "Bad floatingip request: Port `+
					req.FloatingIP.PortID+` does not have fixed ip `+req.FloatingIP.FixedIP+`.", "detail": ""}}`)(w, r)
				return
			}
			portID, fixedIP = req.FloatingIP.PortID, want
			get(w, r)
		},
	}
}

// fipAssocModel is fip-1's association with port-1 as state holds it.
func fipAssocModel() floatingIPAssociateModel {
	return floatingIPAssociateModel{
		ID:           types.StringValue(fipAssocFloatingIPID),
		FloatingIPID: types.StringValue(fipAssocFloatingIPID),
		PortID:       types.StringValue(fipAssocPortID),
		FixedIP:      types.StringValue(fipAssocFixedIP),
		Region:       types.StringValue("region-one"),
	}
}

// A config that moves the floating IP to another port and leaves fixed_ip
// unset used to plan the old port's address, so Update sent it with the new
// port and Neutron refused the move. The address belongs to the old port, so
// the plan must leave it unknown, and Neutron picks the new port's.
func TestFloatingIPAssociateUpdateToAnotherPortDropsTheOldFixedIP(t *testing.T) {
	t.Parallel()
	r := &floatingIPAssociateResource{config: newFakeNeutron(t, movingFIPAssocRoutes(t)).config}
	s := schemaOf(t, r)

	prior := fipAssocModel()
	planned := prior
	planned.PortID = types.StringValue(fipAssocNewPortID)
	planned.FixedIP = types.StringUnknown()
	fixedIP, _ := planUpdate(t, r, "fixed_ip", &prior, &planned)
	planned.FixedIP = fixedIP.(types.String)

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update with fixed_ip planned as %s: %v", planned.FixedIP, resp.Diagnostics)
	}
	var got floatingIPAssociateModel
	if d := resp.State.Get(t.Context(), &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.PortID.ValueString() != fipAssocNewPortID || got.FixedIP.ValueString() != fipAssocNewPortFixedIP {
		t.Fatalf("update state port_id=%s fixed_ip=%s; want %s, %s", got.PortID, got.FixedIP, fipAssocNewPortID, fipAssocNewPortFixedIP)
	}
}

// While the port stays the same, an unset fixed_ip keeps its address from
// state, so a plan that changes nothing else shows no change for it.
func TestFloatingIPAssociateFixedIPKeepsStateForTheSamePort(t *testing.T) {
	t.Parallel()
	r := &floatingIPAssociateResource{}
	prior := fipAssocModel()
	planned := prior
	planned.Region = types.StringValue("region-two")
	planned.FixedIP = types.StringUnknown()

	if fixedIP, _ := planUpdate(t, r, "fixed_ip", &prior, &planned); !fixedIP.Equal(prior.FixedIP) {
		t.Fatalf("fixed_ip planned as %s, want the state's %s", fixedIP, prior.FixedIP)
	}
}
