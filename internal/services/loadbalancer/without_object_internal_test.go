// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"

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

// gophercloud decodes a create answer whose body lacks the object's key to a
// nil object and no error. The creates used to dereference it and crash the
// provider. Each must report an error and leave nothing in state.
func TestCreateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	t.Run("loadbalancer", func(t *testing.T) {
		t.Parallel()
		r := &loadBalancerResource{config: newFakeOctavia(t, octaviaRoutes{
			"POST " + lbsPath: reply(http.StatusCreated, `{}`),
		}).config}
		planned := plannedLoadBalancer()
		var resp resource.CreateResponse
		noPanic(t, func() { resp = runCreate(r, newPlan(t, schemaOf(t, r), &planned)) })
		if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
			t.Fatalf("create diagnostics %v, state %v; want an error and no state", resp.Diagnostics, resp.State.Raw)
		}
	})
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			routes := c.routes()
			routes["POST "+strings.TrimSuffix(c.collection, "/")] = reply(http.StatusCreated, `{}`)
			r := c.resource(newFakeOctavia(t, routes).config)
			var resp resource.CreateResponse
			noPanic(t, func() { resp = runCreate(r, newPlan(t, schemaOf(t, r), c.planned)) })
			if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
				t.Fatalf("create diagnostics %v, state %v; want an error and no state", resp.Diagnostics, resp.State.Raw)
			}
		})
	}
}

// A 200 for the load balancer's own URL whose body holds no loadbalancer
// object makes gophercloud return no load balancer and no error. The settle
// and delete waits used to dereference it; each must fail on it instead.
func TestLoadBalancerWaitsRefuseAnAnswerWithoutTheLoadBalancer(t *testing.T) {
	t.Parallel()
	waits := map[string]func(context.Context, *clients.Config) error{
		"settled": func(ctx context.Context, config *clients.Config) error {
			client, err := config.LoadBalancerV2Client()
			if err != nil {
				return err
			}
			_, err = waitForLoadBalancerSettled(ctx, client, "lb-1", 2*time.Second)
			return err
		},
		"deleted": func(ctx context.Context, config *clients.Config) error {
			client, err := config.LoadBalancerV2Client()
			if err != nil {
				return err
			}
			return waitForLoadBalancerDeleted(ctx, client, "lb-1", 2*time.Second)
		},
	}
	for name, wait := range waits {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			octavia := newFakeOctavia(t, octaviaRoutes{"GET " + lbPath: reply(http.StatusOK, lbList)})
			var err error
			noPanic(t, func() { err = wait(context.Background(), octavia.config) })
			if !errors.Is(err, clients.ErrNoObject) {
				t.Fatalf("wait error = %v; want it to report the answer without the object", err)
			}
		})
	}
}

// A pool whose listener_id names its parent resolves its load balancer through
// that listener. A 200 for the listener's URL whose body holds no listener
// object makes gophercloud return no listener and no error, which the lookup
// used to dereference. It must fail on it instead.
func TestRootLBIDFromListenerRefusesAnAnswerWithoutTheListener(t *testing.T) {
	t.Parallel()
	octavia := newFakeOctavia(t, octaviaRoutes{
		"GET " + listenerPath: reply(http.StatusOK, `{"listeners": [`+listenerJSON+`], "listeners_links": []}`),
	})
	client, err := octavia.config.LoadBalancerV2Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	noPanic(t, func() { _, err = rootLBIDFromListener(context.Background(), client, "listener-1") })
	if !errors.Is(err, clients.ErrNoObject) {
		t.Fatalf("lookup error = %v; want it to report the answer without the object", err)
	}
}
