// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package keymanager

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

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

// gophercloud decodes a Barbican answer as the object itself, so a 2xx whose
// body is JSON null decodes to a nil object and no error. The secret and
// container creates used to dereference it and crash the provider. Each must
// report an error and leave nothing in state.
func TestCreateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	creates := []struct {
		name, route string
		newResource func(*clients.Config) resource.Resource
		plan        func(*testing.T, resource.Resource) tfsdk.Plan
	}{
		{"secret", "POST " + secretsPath,
			func(c *clients.Config) resource.Resource { return &secretResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan {
				return newPlan(t, schemaOf(t, r), &secretModel{
					ID: types.StringUnknown(), Name: types.StringValue("tf-acc-secret"), Algorithm: types.StringUnknown(),
					BitLength: types.Int64Unknown(), Mode: types.StringUnknown(), SecretType: types.StringValue("passphrase"),
					Expiration: types.StringUnknown(), Payload: types.StringValue("s3cr3t-passphrase"),
					PayloadContentType: types.StringValue("text/plain"), PayloadContentEncoding: types.StringNull(),
					SecretRef: types.StringUnknown(), Status: types.StringUnknown(), CreatorID: types.StringUnknown(),
					ContentTypes: types.MapUnknown(types.StringType), CreatedAt: types.StringUnknown(),
					UpdatedAt: types.StringUnknown(), Region: types.StringUnknown(),
				})
			}},
		{"container", "POST " + containersPath,
			func(c *clients.Config) resource.Resource { return &containerResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan { return containerPlan(t, r.(*containerResource)) }},
	}
	for _, c := range creates {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.newResource(newFakeBarbican(t, barbicanRoutes{c.route: reply(http.StatusCreated, `null`)}).config)
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

// waitForSecretActive polls the secret, and used to dereference the nil secret
// a JSON null 200 decodes to. It must fail on it instead.
func TestWaitForSecretActiveRefusesAnAnswerWithoutTheSecret(t *testing.T) {
	t.Parallel()
	client, err := newFakeBarbican(t, barbicanRoutes{"GET " + secretPath: reply(http.StatusOK, `null`)}).config.KeyManagerV1Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	noPanic(t, func() { err = waitForSecretActive(context.Background(), client, "sec-1", 2*time.Second) })
	if !errors.Is(err, clients.ErrNoObject) {
		t.Fatalf("wait error = %v; want it to report the answer without the object", err)
	}
}
