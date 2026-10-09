// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// invokeRebuildAction runs the action's Invoke the way Terraform does, with a
// config built from the action schema, and collects the progress messages.
// SendProgress must be set: the framework supplies it, and a nil func panics.
func invokeRebuildAction(t *testing.T, f *rebuildFake) (action.InvokeResponse, []string) {
	t.Helper()
	ctx := context.Background()
	a := &instanceRebuildAction{config: f.start().config}
	var sch action.SchemaResponse
	a.Schema(ctx, action.SchemaRequest{}, &sch)
	if sch.Diagnostics.HasError() {
		t.Fatalf("schema: %v", sch.Diagnostics)
	}
	cfg := tfsdk.Config{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), map[string]tftypes.Value{
		"instance_id": tftypes.NewValue(tftypes.String, "srv-1"),
		"region":      tftypes.NewValue(tftypes.String, nil),
	})}
	var progress []string
	resp := action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) { progress = append(progress, e.Message) }}
	a.Invoke(ctx, action.InvokeRequest{Config: cfg}, &resp)
	return resp, progress
}

func TestInstanceRebuildActionMetadata(t *testing.T) {
	var resp action.MetadataResponse
	(&instanceRebuildAction{}).Metadata(context.Background(), action.MetadataRequest{ProviderTypeName: "pcd"}, &resp)
	if resp.TypeName != "pcd_compute_instance_rebuild" {
		t.Fatalf("type name %q", resp.TypeName)
	}
}

// The action reimages with the image the instance runs, and reports progress.
func TestInstanceRebuildActionReimagesWithCurrentImage(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-1"),
		after: []string{
			rebuildServerJSON("REBUILD", "rebuilding", ""),
			rebuildServerJSON("ACTIVE", "", "img-1"),
		},
		images: map[string]string{"img-1": rebuildImageJSON("img-1", "cirros", "")},
	}
	resp, progress := invokeRebuildAction(t, f)
	if resp.Diagnostics.HasError() {
		t.Fatalf("invoke: %v", resp.Diagnostics)
	}
	if len(f.rebuilds) != 1 {
		t.Fatalf("%d rebuild requests, want 1", len(f.rebuilds))
	}
	body, _ := json.Marshal(f.rebuilds[0])
	if string(body) != `{"rebuild":{"imageRef":"img-1"}}` {
		t.Errorf("rebuild body %s, want the current image only", body)
	}
	if len(progress) < 2 {
		t.Errorf("progress messages %v; want a start and an end message", progress)
	}
}

// A stopped instance is waited for until it is stopped again, and the
// progress ticker reports while the wait runs.
func TestInstanceRebuildActionWaitsForShutoff(t *testing.T) {
	rebuildFastPolls(t)
	old := actionProgressInterval
	actionProgressInterval = time.Millisecond
	t.Cleanup(func() { actionProgressInterval = old })
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("SHUTOFF", "", "img-1"),
		after: []string{
			rebuildServerJSON("REBUILD", "rebuilding", ""),
			rebuildServerJSON("ACTIVE", "", "img-1"),
			rebuildServerJSON("SHUTOFF", "", "img-1"),
		},
		images: map[string]string{"img-1": rebuildImageJSON("img-1", "cirros", "")},
	}
	resp, progress := invokeRebuildAction(t, f)
	if resp.Diagnostics.HasError() {
		t.Fatalf("invoke: %v", resp.Diagnostics)
	}
	if f.afterGets < len(f.after) {
		t.Errorf("the wait stopped after %d of %d reads; it accepted the transient ACTIVE", f.afterGets, len(f.after))
	}
	if last := progress[len(progress)-1]; !strings.Contains(last, "SHUTOFF") {
		t.Errorf("last progress message %q does not report SHUTOFF", last)
	}
}

// A refusal says how to run the action again, not the resource's advice: a
// plain apply does not run an action whose trigger has already fired, and the
// action has no power_state step to resume an instance in the same apply.
func TestInstanceRebuildActionRefusals(t *testing.T) {
	const rerun = "-invoke=action.pcd_compute_instance_rebuild"
	for _, tc := range []struct {
		name   string
		server string
		want   string
	}{
		{name: "paused", server: rebuildServerJSON("PAUSED", "", "img-1"), want: rerun},
		{name: "task in progress", server: rebuildServerJSON("ACTIVE", "image_uploading", "img-1"), want: rerun},
		{name: "rescued", server: rebuildServerJSON("RESCUE", "", "img-1"), want: rerun},
		{name: "volume-backed without a bootable volume", server: rebuildServerJSON("ACTIVE", "", ""), want: "no bootable volume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rebuildFastPolls(t)
			f := &rebuildFake{t: t, before: tc.server}
			resp, _ := invokeRebuildAction(t, f)
			if !resp.Diagnostics.HasError() {
				t.Fatal("invoke succeeded; want it refused")
			}
			if len(f.rebuilds) != 0 {
				t.Fatal("a rebuild request was sent")
			}
			detail := resp.Diagnostics.Errors()[0].Detail()
			if !strings.Contains(detail, tc.want) {
				t.Errorf("error %q does not mention %q", detail, tc.want)
			}
			for _, resourceOnly := range []string{"Apply again", "same apply"} {
				if strings.Contains(detail, resourceOnly) {
					t.Errorf("error %q gives the resource's advice %q", detail, resourceOnly)
				}
			}
		})
	}
}

// A rebuild that ends in ERROR fails the action.
func TestInstanceRebuildActionEndingInError(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-1"),
		after: []string{
			rebuildServerJSON("REBUILD", "rebuilding", ""),
			rebuildServerJSON("ERROR", "", "img-1"),
		},
		images: map[string]string{"img-1": rebuildImageJSON("img-1", "cirros", "")},
	}
	resp, _ := invokeRebuildAction(t, f)
	if !resp.Diagnostics.HasError() {
		t.Fatal("invoke succeeded although the rebuild ended in ERROR")
	}
}
