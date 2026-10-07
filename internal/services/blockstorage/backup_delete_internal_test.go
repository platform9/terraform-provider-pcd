// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// runBackupDelete deletes backup bk-1 through a Cinder that answers DELETE and
// GET for it with deleteAnswer and getAnswer, and returns the response.
func runBackupDelete(t *testing.T, deleteAnswer, getAnswer http.HandlerFunc) resource.DeleteResponse {
	t.Helper()
	ctx := context.Background()
	cinder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "DELETE /backups/bk-1":
			deleteAnswer(w, r)
		case "GET /backups/bk-1":
			getAnswer(w, r)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(cinder.Close)

	r := &backupResource{config: fakeConfig(cinder.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &backupModel{
		ID: types.StringValue("bk-1"), VolumeID: types.StringValue("vol-1"), Name: types.StringValue("nightly"),
		Description: types.StringValue(""), Incremental: types.BoolValue(false), Force: types.BoolValue(false),
		Container: types.StringValue(""), Size: types.Int64Value(1), Status: types.StringValue("error"),
		Region: types.StringValue("region-one"),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
	return resp
}

// answer replies with status and a JSON body.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// On a cloud without a backup service, Cinder answers a backup DELETE with 404
// and "Service cinder-backup could not be found", while the backup stays.
// Delete used to take any 404 for a backup already gone, so Terraform forgot a
// backup that still existed. It must report the error.
func TestBackupDeleteReportsA404ForAMissingService(t *testing.T) {
	t.Parallel()
	resp := runBackupDelete(t,
		answer(http.StatusNotFound, `{"itemNotFound": {"code": 404, "message": "Service cinder-backup could not be found."}}`),
		answer(http.StatusOK, `{"backup": {"id": "bk-1", "name": "nightly", "volume_id": "vol-1", "status": "error",
			"fail_reason": "Service not found for creating backup.", "size": 1}}`))
	if !resp.Diagnostics.HasError() {
		t.Fatal("delete succeeded while the backup still exists; Terraform forgets it")
	}
}

// A 404 for a backup that really is gone stays a success.
func TestBackupDeleteAcceptsA404ForABackupAlreadyGone(t *testing.T) {
	t.Parallel()
	notFound := answer(http.StatusNotFound, `{"itemNotFound": {"code": 404, "message": "Backup bk-1 could not be found."}}`)
	if resp := runBackupDelete(t, notFound, notFound); resp.Diagnostics.HasError() {
		t.Fatalf("delete of a backup already gone: %v", resp.Diagnostics)
	}
}
