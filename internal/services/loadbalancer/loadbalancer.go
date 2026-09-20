// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package loadbalancer implements the pcd_lb_* resources and data sources
// (Octavia load balancer v2), ported from terraform-provider-openstack v3.4.0.
//
// Octavia serializes changes per load balancer: after any create/update/delete of
// the load balancer or one of its children, the root load balancer enters a
// PENDING_* provisioning status and the API rejects further changes with HTTP 409
// until it returns to ACTIVE. Every create and update waits for the root load
// balancer to be ACTIVE before and after the mutation, using
// waitForLoadBalancerActive, and a create still refuses to build on a root
// that is in ERROR.
//
// A child's delete (listener, pool, member, monitor) instead waits only for
// the root to settle out of PENDING_*, using waitForLoadBalancerSettled.
// Octavia's own immutability check (verified against its source, see that
// function's doc comment) accepts a child mutation only while the root is
// ACTIVE, not merely "not PENDING_*", so a root in ERROR still answers a
// child's delete with HTTP 409 -- nothing in this package can change that.
// Settling on ERROR only stops the wait itself from being the failure: the
// child's own delete call reports the 409 if Octavia refuses it, and the real
// benefit is the wait done right after a child's delete succeeds, so a root
// that has since moved to ERROR does not turn an already-successful delete
// into a reported failure. The root load balancer's own delete
// (loadbalancer_resource.go) skips both waits entirely and goes straight to
// a cascade delete.
package loadbalancer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
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
// by the nested resources (member = pool/member).
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

// waitForLoadBalancerSettled blocks until the root load balancer leaves its
// transient PENDING_* provisioning statuses and reports the status it settled
// on. ACTIVE, ERROR, and DELETED all count as settled, and so does a 404 (the
// load balancer, and everything under it, is already gone). The settled set
// is enumerated explicitly, exactly as in waitForLoadBalancerActive, rather
// than tested with a "PENDING_" prefix: Octavia never promised that every
// transient status is named that way.
//
// Settling in ERROR is not the same as Octavia accepting further changes.
// Verified by reading the source of the lab's deployed Octavia 15.0.1.dev10
// (octavia/common/constants.py and octavia/db/repositories.py): a load
// balancer's own delete is checked against
// DELETABLE_STATUSES = (ACTIVE, ERROR), but every child controller (listener,
// pool, member, monitor) requests PENDING_UPDATE as the load balancer's
// target status when deleting a child, which is checked against
// MUTABLE_STATUSES = (ACTIVE,) instead. A child's delete issued while the
// root is in ERROR is therefore refused with HTTP 409 -- this waiter does not
// change that, and cannot. Its purpose is narrower: a root in ERROR should be
// reported by the child's own delete call, with a specific 409, rather than
// by this wait failing first with a generic "did not reach ACTIVE" message,
// and a root that moved to ERROR only after a child's delete already
// succeeded should not be mistaken for a failed delete. See
// settleForChildDelete, which callers use instead of calling this directly.
func waitForLoadBalancerSettled(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status := ""
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		lb, err := loadbalancers.Get(ctx, client, lbID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				status = lbDeleted
				return true, nil
			}
			return false, err
		}
		status = lb.ProvisioningStatus
		switch lb.ProvisioningStatus {
		case lbActive, lbError, lbDeleted:
			return true, nil
		default: // PENDING_CREATE / PENDING_UPDATE / PENDING_DELETE
			return false, nil
		}
	})
	if err != nil {
		return status, fmt.Errorf("waiting for load balancer %s to settle: %w", lbID, err)
	}
	return status, nil
}

// settleForChildDelete waits for the root load balancer to leave its
// transient PENDING_* status, for use immediately before or immediately after
// a child's own delete call, and reports whether the caller should continue.
// false means it has already added an error diagnostic and the caller should
// return. phase is "before" or "after", naming which of the child Delete's
// two waits is calling, so a failure of the second wait is not misreported as
// a failure of the first.
//
// A root that settles in ERROR is not treated as an error here. As documented
// on waitForLoadBalancerSettled, Octavia still refuses a child's delete
// against a root in ERROR with HTTP 409, and that refusal is reported by the
// child's own delete call, not by this function. This only adds a warning
// naming the load balancer, telling the operator to repair or delete it and
// retry the destroy.
func settleForChildDelete(ctx context.Context, client *gophercloud.ServiceClient, lbID, child, phase string, diags *diag.Diagnostics) bool {
	status, err := waitForLoadBalancerSettled(ctx, client, lbID, defaultLBTimeout)
	if err != nil {
		diags.AddError(fmt.Sprintf("loadbalancer: waiting %s %s delete", phase, child), err.Error())
		return false
	}
	if status == lbError {
		diags.AddWarning("Load balancer in ERROR provisioning status",
			fmt.Sprintf("Load balancer %s is in ERROR provisioning status. Octavia will likely refuse to delete "+
				"the %s while the load balancer remains in this state. Repair or delete the load balancer and "+
				"retry the destroy.", lbID, child))
	}
	return true
}

// waitForLoadBalancerDeleted blocks until the load balancer is gone: a GET that
// answers 404 and a provisioning status of DELETED both count as gone. A load
// balancer a failed create wait abandoned in ERROR is still deletable, and
// Octavia can still report ERROR on the first poll after the DELETE is accepted,
// so ERROR counts as a delete failure only once the load balancer has been seen
// leaving it. Used after a cascade delete of the root load balancer.
func waitForLoadBalancerDeleted(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lastStatus, seenNonError := "", false
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		lb, err := loadbalancers.Get(ctx, client, lbID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		lastStatus = lb.ProvisioningStatus
		if lb.ProvisioningStatus == lbDeleted {
			return true, nil
		}
		if lb.ProvisioningStatus != lbError {
			seenNonError = true
			return false, nil
		}
		if seenNonError {
			return false, fmt.Errorf("load balancer %s entered ERROR provisioning status during delete", lbID)
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for load balancer %s to delete (last status %q): %w", lbID, lastStatus, err)
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
