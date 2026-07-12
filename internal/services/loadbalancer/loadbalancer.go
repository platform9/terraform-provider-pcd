// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package loadbalancer implements the pcd_lb_* resources and data sources
// (Octavia load balancer v2), ported from terraform-provider-openstack v3.4.0.
//
// Octavia serializes changes per load balancer: after any create/update/delete of
// the load balancer or one of its children, the root load balancer enters a
// PENDING_* provisioning status and the API rejects further changes with HTTP 409
// until it returns to ACTIVE. Every resource therefore waits for the root load
// balancer to be ACTIVE before and after each mutation, using waitForLoadBalancerActive.
package loadbalancer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// intPtrIfSet returns a *int for a known, non-null Int64 attribute, else nil so
// the corresponding request field is omitted and the server default applies.
func intPtrIfSet(v types.Int64) *int {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	x := int(v.ValueInt64())
	return &x
}

// boolPtrIfSet returns a *bool for a known, non-null Bool attribute, else nil.
func boolPtrIfSet(v types.Bool) *bool {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	x := v.ValueBool()
	return &x
}

// listToStrings converts a list attribute to a Go slice (nil for null/unknown).
func listToStrings(ctx context.Context, l types.List, diags *diag.Diagnostics) []string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return out
}

// mapToStrings converts a map attribute to a Go map (nil for null/unknown).
func mapToStrings(ctx context.Context, m types.Map, diags *diag.Diagnostics) map[string]string {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	out := map[string]string{}
	diags.Append(m.ElementsAs(ctx, &out, false)...)
	return out
}

// splitParentChildID parses a composite "<parent_id>/<child_id>" import ID used
// by the nested resources (member = pool/member, l7rule = policy/rule).
func splitParentChildID(id string) (parent, child string, err error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected import ID in the form <parent_id>/<child_id>, got %q", id)
	}
	return parts[0], parts[1], nil
}

// Octavia provisioning-status values. The gophercloud package exposes these only
// as documentation, not as exported constants, so they are declared here.
const (
	lbActive  = "ACTIVE"
	lbError   = "ERROR"
	lbDeleted = "DELETED"
)

// defaultLBTimeout bounds each wait for the load balancer to settle.
const defaultLBTimeout = 10 * time.Minute

// configureClient extracts the shared *clients.Config from ProviderData.
func configureClient(providerData any, diags *diag.Diagnostics) *clients.Config {
	if providerData == nil {
		return nil
	}
	config, ok := providerData.(*clients.Config)
	if !ok {
		diags.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *clients.Config, got %T. This is a bug in the provider.", providerData),
		)
		return nil
	}
	return config
}

// waitForLoadBalancerActive blocks until the load balancer reaches ACTIVE,
// failing on ERROR/DELETED or timeout. PENDING_* statuses are transient.
func waitForLoadBalancerActive(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		lb, err := loadbalancers.Get(ctx, client, lbID).Extract()
		if err != nil {
			return false, err
		}
		switch lb.ProvisioningStatus {
		case lbActive:
			return true, nil
		case lbError, lbDeleted:
			return false, fmt.Errorf("load balancer %s entered %s provisioning status", lbID, lb.ProvisioningStatus)
		default:
			return false, nil
		}
	})
	if err != nil {
		return fmt.Errorf("waiting for load balancer %s to become ACTIVE: %w", lbID, err)
	}
	return nil
}

// waitForLoadBalancerDeleted blocks until the load balancer is gone (404). Used
// after a cascade delete of the root load balancer.
func waitForLoadBalancerDeleted(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		lb, err := loadbalancers.Get(ctx, client, lbID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		if lb.ProvisioningStatus == lbError {
			return false, fmt.Errorf("load balancer %s entered ERROR provisioning status during delete", lbID)
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for load balancer %s to delete: %w", lbID, err)
	}
	return nil
}

// rootLBIDFromListener resolves the load balancer a listener belongs to.
func rootLBIDFromListener(ctx context.Context, client *gophercloud.ServiceClient, listenerID string) (string, error) {
	l, err := listeners.Get(ctx, client, listenerID).Extract()
	if err != nil {
		return "", err
	}
	if len(l.Loadbalancers) == 0 {
		return "", fmt.Errorf("listener %s is not attached to a load balancer", listenerID)
	}
	return l.Loadbalancers[0].ID, nil
}

// rootLBIDFromPool resolves the load balancer a pool belongs to, directly or via
// its listener.
func rootLBIDFromPool(ctx context.Context, client *gophercloud.ServiceClient, poolID string) (string, error) {
	p, err := pools.Get(ctx, client, poolID).Extract()
	if err != nil {
		return "", err
	}
	if len(p.Loadbalancers) > 0 {
		return p.Loadbalancers[0].ID, nil
	}
	if len(p.Listeners) > 0 {
		return rootLBIDFromListener(ctx, client, p.Listeners[0].ID)
	}
	return "", fmt.Errorf("pool %s is not attached to a load balancer or listener", poolID)
}

// rootLBIDFromL7Policy resolves the load balancer an L7 policy belongs to via its
// listener.
func rootLBIDFromL7Policy(ctx context.Context, client *gophercloud.ServiceClient, policyID string) (string, error) {
	p, err := l7policies.Get(ctx, client, policyID).Extract()
	if err != nil {
		return "", err
	}
	if p.ListenerID == "" {
		return "", fmt.Errorf("l7 policy %s is not attached to a listener", policyID)
	}
	return rootLBIDFromListener(ctx, client, p.ListenerID)
}
