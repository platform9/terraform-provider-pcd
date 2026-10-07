// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package keymanager

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Every attribute forces a new container, so Update is not expected to run.
// consumers has no plan modifier, so a plan that reached it would hold
// consumers unknown, and Update used to save the plan as it was, which
// Terraform rejects. It must keep the prior consumers.
func TestContainerUpdateKeepsStateKnown(t *testing.T) {
	t.Parallel()
	barbican := newFakeBarbican(t, containerRoutes())
	r := &containerResource{config: barbican.config}
	s := schemaOf(t, r)

	prior := containerModel{
		ID:           types.StringValue(containerID),
		Name:         types.StringValue(containerName),
		Type:         types.StringValue("generic"),
		SecretRefs:   containerSecretRefs(t),
		ContainerRef: types.StringValue(containerRef),
		Status:       types.StringValue("ACTIVE"),
		Consumers:    types.ListValueMust(containerConsumerObjType, nil),
		CreatedAt:    types.StringValue("2026-10-02T00:00:00Z"),
		Region:       types.StringValue("region-one"),
	}
	planned := prior
	planned.Consumers = types.ListUnknown(containerConsumerObjType)

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("update state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	var got containerModel
	if d := resp.State.Get(t.Context(), &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if !got.Consumers.Equal(prior.Consumers) {
		t.Fatalf("update state consumers = %s, want the prior consumers", got.Consumers)
	}
}
