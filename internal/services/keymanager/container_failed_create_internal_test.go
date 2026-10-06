// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package keymanager

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	containerID   = "ctr-1"
	containerName = "workload-secrets"
	containerRef  = "http://barbican.invalid/v1/containers/ctr-1"

	containersPath = "/v1/containers"
	containerPath  = containersPath + "/" + containerID
)

// containerJSON is ctr-1 as Barbican returns it: the body is the container
// itself, with no wrapping key, and it holds the one secret the tests
// configure. Barbican's timestamps carry no zone, and gophercloud parses them
// only in that form.
const containerJSON = `{"container_ref": "http://barbican.invalid/v1/containers/ctr-1",
	"name": "workload-secrets", "type": "generic", "status": "ACTIVE", "creator_id": "user-1",
	"secret_refs": [{"name": "passphrase", "secret_ref": "http://barbican.invalid/v1/secrets/sec-1"}],
	"consumers": [], "created": "2026-10-02T00:00:00", "updated": "2026-10-02T00:00:00"}`

// containerListJSON is Barbican's answer for the collection URL: a page of
// containers, not a container.
const containerListJSON = `{"containers": [` + containerJSON + `], "total": 1}`

// containerRoutes answers every call containerResource makes for ctr-1 the way
// Barbican does. Each test replaces the route whose failure it exercises.
func containerRoutes() barbicanRoutes {
	return barbicanRoutes{
		"POST " + containersPath:  reply(http.StatusCreated, `{"container_ref": "`+containerRef+`"}`),
		"GET " + containerPath:    reply(http.StatusOK, containerJSON),
		"DELETE " + containerPath: reply(http.StatusNoContent, ""),
	}
}

// containerSecretRefs is the one secret_refs entry the tests configure.
func containerSecretRefs(t *testing.T) types.Set {
	t.Helper()
	refs, d := types.SetValueFrom(context.Background(), containerSecretRefObjType, []containerSecretRefModel{{
		SecretRef: types.StringValue("http://barbican.invalid/v1/secrets/sec-1"),
		Name:      types.StringValue("passphrase"),
	}})
	if d.HasError() {
		t.Fatalf("building the secret_refs: %v", d)
	}
	return refs
}

// containerPlan is the plan the framework computes for a new container whose
// config sets name, type and one secret_refs entry. The Computed attributes the
// config leaves unset are unknown.
func containerPlan(t *testing.T, r *containerResource) tfsdk.Plan {
	t.Helper()
	return newPlan(t, schemaOf(t, r), &containerModel{
		ID:           types.StringUnknown(),
		Name:         types.StringValue(containerName),
		Type:         types.StringValue("generic"),
		SecretRefs:   containerSecretRefs(t),
		ContainerRef: types.StringUnknown(),
		Status:       types.StringUnknown(),
		Consumers:    types.ListUnknown(containerConsumerObjType),
		CreatedAt:    types.StringUnknown(),
		Region:       types.StringUnknown(),
	})
}

// containerRowWithoutID is the row provider v0.1.14 left in state when the
// read-back after a successful POST failed: Terraform saved the unknowns Create
// returned as null, the ID among them. v0.1.14's next refresh of that row read
// the collection URL, took the list for an empty container and saved id "", so
// id is null or empty. Read and Delete look only at the id.
func containerRowWithoutID(t *testing.T, r *containerResource, id types.String) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &containerModel{
		ID:           id,
		Name:         types.StringValue(containerName),
		Type:         types.StringValue("generic"),
		SecretRefs:   containerSecretRefs(t),
		ContainerRef: types.StringNull(),
		Status:       types.StringNull(),
		Consumers:    types.ListNull(containerConsumerObjType),
		CreatedAt:    types.StringNull(),
		Region:       types.StringNull(),
	})
}

// containerRow returns the container model state holds.
func containerRow(t *testing.T, state tfsdk.State) containerModel {
	t.Helper()
	var m containerModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the container fully known.
func TestContainerCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	barbican := newFakeBarbican(t, containerRoutes())
	r := &containerResource{config: barbican.config}

	resp := runCreate(r, containerPlan(t, r))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := containerRow(t, resp.State)
	if got.ID.ValueString() != containerID || got.ContainerRef.ValueString() != containerRef ||
		got.Status.ValueString() != "ACTIVE" || got.CreatedAt.ValueString() != "2026-10-02T00:00:00Z" ||
		got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s container_ref=%s status=%s created_at=%s region=%s; "+
			"want ctr-1, its ref, ACTIVE, 2026-10-02T00:00:00Z, region-one",
			got.ID, got.ContainerRef, got.Status, got.CreatedAt, got.Region)
	}
	want := []string{"POST " + containersPath, "GET " + containerPath}
	if sent := barbican.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Barbican keeps a container whose read-back fails. Create used to write the
// plan's unknowns, which Terraform saved as null, ID included, so the container
// was orphaned and the next refresh read the collection URL and saved id "".
// Create must return the error with the container's ID and ref in state
// (Terraform then taints it), and a refresh must fill in what the read-back
// would have.
func TestContainerCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := containerRoutes()
	routes["GET "+containerPath] = badGatewayFirst(routes["GET "+containerPath])
	r := &containerResource{config: newFakeBarbican(t, routes).config}

	createResp := runCreate(r, containerPlan(t, r))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets ctr-1 and the next apply creates a second container")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	got := containerRow(t, createResp.State)
	if got.ID.ValueString() != containerID || got.ContainerRef.ValueString() != containerRef {
		t.Fatalf("create state id=%s container_ref=%s; want ctr-1 and the ref Barbican returned: "+
			"a row without them names nothing a destroy could delete", got.ID, got.ContainerRef)
	}

	// The replacing apply, or a destroy, refreshes the tainted container first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded container: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped ctr-1 from state")
	}
	got = containerRow(t, readResp.State)
	if got.ID.ValueString() != containerID || got.Status.ValueString() != "ACTIVE" ||
		got.CreatedAt.ValueString() == "" || got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s status=%s created_at=%s region=%s; want ctr-1, ACTIVE, the server's time, region-one",
			got.ID, got.Status, got.CreatedAt, got.Region)
	}
}

// A read-back that finds the container gone right after the POST used to be
// swallowed: Create reported nothing and Terraform saved a row with no ID.
// Create must report it and leave nothing in state.
func TestContainerCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := containerRoutes()
	routes["GET "+containerPath] = reply(http.StatusNotFound, "")
	r := &containerResource{config: newFakeBarbican(t, routes).config}

	resp := runCreate(r, containerPlan(t, r))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a container the read-back found gone: %v", resp.State.Raw)
	}
}

// The row v0.1.14 left has no ID. Read used to send GET for an empty ID, which
// reaches the collection URL; Barbican answers that with the list, which
// gophercloud decodes as an empty container, and Read saved it back as id "".
// Read must drop the row with a warning that names the container, without
// sending anything.
func TestContainerReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := containerRoutes()
	routes["GET "+containersPath+"/"] = reply(http.StatusOK, containerListJSON)
	barbican := newFakeBarbican(t, routes)
	r := &containerResource{config: barbican.config}

	for _, id := range []types.String{types.StringNull(), types.StringValue("")} {
		resp := runRead(r, containerRowWithoutID(t, r, id))
		if sent := barbican.received(); len(sent) != 0 {
			t.Fatalf("id %s: refresh of a row with no ID sent %v; it names nothing to read", id, sent)
		}
		if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
			t.Fatalf("id %s: diagnostics = %v, want exactly one warning", id, resp.Diagnostics)
		}
		if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-secrets"`) {
			t.Fatalf("id %s: warning detail %q does not name the container to look for", id, detail)
		}
		if !resp.State.Raw.IsNull() {
			t.Fatalf("id %s: refresh kept a row with no ID: %v", id, resp.State.Raw)
		}
	}
}

// containers.Extract decodes the whole body as the container, so a 200 for the
// container's own URL whose body is not a container gives an empty one and no
// error. readInto must report that as an error, and not as not-found: that
// would drop a real container from state.
func TestContainerReadIntoRefusesAnAnswerWithoutTheContainer(t *testing.T) {
	t.Parallel()
	routes := containerRoutes()
	routes["GET "+containerPath] = reply(http.StatusOK, containerListJSON)
	r := &containerResource{config: newFakeBarbican(t, routes).config}
	client, err := r.config.KeyManagerV1Client()
	if err != nil {
		t.Fatalf("building the fake Barbican client: %v", err)
	}

	m := containerModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, containerID, &m)
	if notFound {
		t.Fatal("readInto reported ctr-1 not found; the next refresh would drop a container that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the container: diagnostics %v", diags)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the
// v0.1.14 row first, and so does the replace that follows v0.1.14's refresh of
// it. Delete used to send DELETE for an empty ID, which reaches the collection
// URL, and Barbican refuses that. Delete must warn and send nothing, so
// Terraform forgets the row.
func TestContainerDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := containerRoutes()
	routes["DELETE "+containersPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	barbican := newFakeBarbican(t, routes)
	r := &containerResource{config: barbican.config}

	for _, id := range []types.String{types.StringNull(), types.StringValue("")} {
		resp := runDelete(r, containerRowWithoutID(t, r, id))
		if sent := barbican.received(); len(sent) != 0 {
			t.Fatalf("id %s: delete of a row with no ID sent %v; it names nothing to delete", id, sent)
		}
		if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
			t.Fatalf("id %s: diagnostics = %v, want exactly one warning", id, resp.Diagnostics)
		}
	}
}
