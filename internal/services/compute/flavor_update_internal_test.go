// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// extra_specs has no plan modifier, so a config that leaves it unset would plan
// it unknown in any update. Update used to read that as "no extra specs" and
// delete every spec the flavor had. It must leave the specs the config does
// not manage alone.
func TestFlavorUpdateKeepsUnmanagedExtraSpecs(t *testing.T) {
	t.Parallel()
	nova := newFakeNova(t, flavorRoutes())
	r := &flavorResource{config: nova.config}
	s := schemaOf(t, r)

	prior := flavorInState(t, map[string]string{"hw:cpu_policy": "dedicated"})
	planned := prior
	planned.ExtraSpecs = types.MapUnknown(types.StringType)

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	for _, call := range nova.received() {
		if strings.HasPrefix(call, "DELETE ") {
			t.Fatalf("the update sent %s, deleting an extra spec the config does not manage", call)
		}
	}
	if got := flavorRow(t, resp.State); !got.ExtraSpecs.Equal(prior.ExtraSpecs) {
		t.Fatalf("update state extra_specs = %s, want %s", got.ExtraSpecs, prior.ExtraSpecs)
	}
}

// An update that fails after deleting an extra spec keeps the prior state, so
// the spec is still listed there. A retry run with -refresh=false plans the
// same delete again, and Nova answers it with 404. A spec that is already gone
// is what the delete wants, so the retry must go on.
func TestFlavorUpdateRetryToleratesAnExtraSpecAlreadyDeleted(t *testing.T) {
	t.Parallel()
	routes := flavorRoutes()
	routes["DELETE "+extraSpecsPath+"/hw:cpu_policy"] = reply(http.StatusNotFound,
		`{"itemNotFound": {"code": 404, "message": "Flavor fl-1 has no extra specs with key hw:cpu_policy."}}`)
	routes["GET "+extraSpecsPath] = reply(http.StatusOK, `{"extra_specs": {}}`)
	nova := newFakeNova(t, routes)
	r := &flavorResource{config: nova.config}
	s := schemaOf(t, r)

	prior := flavorInState(t, map[string]string{"hw:cpu_policy": "dedicated"})
	planned := prior
	planned.ExtraSpecs = stringMap(t, map[string]string{})

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if got := flavorRow(t, resp.State); !got.ExtraSpecs.Equal(planned.ExtraSpecs) {
		t.Fatalf("update state extra_specs = %s, want none", got.ExtraSpecs)
	}
	want := []string{"DELETE " + extraSpecsPath + "/hw:cpu_policy", "GET " + extraSpecsPath}
	if sent := nova.received(); !slices.Equal(sent, want) {
		t.Fatalf("update sent %v, want %v", sent, want)
	}
}
