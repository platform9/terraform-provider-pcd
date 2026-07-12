// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package dns implements the pcd_dns_* resources and data sources (Designate v2),
// ported from terraform-provider-openstack v3.4.0.
//
// Designate zone and recordset operations are asynchronous: create/update return
// the object in a PENDING status that settles to ACTIVE, and delete leaves the
// object PENDING_DELETE until it is gone. Resources wait for the object to reach
// ACTIVE after create/update and to 404 after delete.
package dns

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/dns/v2/recordsets"
	"github.com/gophercloud/gophercloud/v2/openstack/dns/v2/zones"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

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

// setToStrings converts a set attribute to a Go slice (nil for null/unknown).
func setToStrings(ctx context.Context, s types.Set, diags *diag.Diagnostics) []string {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(s.ElementsAs(ctx, &out, false)...)
	return out
}

// splitZoneChildID parses a composite "<zone_id>/<recordset_id>" import ID.
func splitZoneChildID(id string) (zoneID, child string, err error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected import ID in the form <zone_id>/<recordset_id>, got %q", id)
	}
	return parts[0], parts[1], nil
}

// Designate status values (plain strings in the API; no exported constants).
const (
	dnsActive = "ACTIVE"
	dnsError  = "ERROR"
)

// defaultDNSTimeout bounds each wait for a zone or recordset to settle.
const defaultDNSTimeout = 10 * time.Minute

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

// waitForZoneActive blocks until the zone reaches ACTIVE, failing on ERROR/timeout.
func waitForZoneActive(ctx context.Context, client *gophercloud.ServiceClient, zoneID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		z, err := zones.Get(ctx, client, zoneID).Extract()
		if err != nil {
			return false, err
		}
		switch z.Status {
		case dnsActive:
			return true, nil
		case dnsError:
			return false, fmt.Errorf("zone %s entered ERROR status", zoneID)
		default:
			return false, nil
		}
	})
	if err != nil {
		return fmt.Errorf("waiting for zone %s to become active: %w", zoneID, err)
	}
	return nil
}

// waitForZoneDeleted blocks until the zone is gone (404).
func waitForZoneDeleted(ctx context.Context, client *gophercloud.ServiceClient, zoneID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		z, err := zones.Get(ctx, client, zoneID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		if z.Status == dnsError {
			return false, fmt.Errorf("zone %s entered ERROR status during delete", zoneID)
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for zone %s to delete: %w", zoneID, err)
	}
	return nil
}

// waitForRecordSetActive blocks until the recordset reaches ACTIVE.
func waitForRecordSetActive(ctx context.Context, client *gophercloud.ServiceClient, zoneID, rrID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		rr, err := recordsets.Get(ctx, client, zoneID, rrID).Extract()
		if err != nil {
			return false, err
		}
		switch rr.Status {
		case dnsActive:
			return true, nil
		case dnsError:
			return false, fmt.Errorf("recordset %s entered ERROR status", rrID)
		default:
			return false, nil
		}
	})
	if err != nil {
		return fmt.Errorf("waiting for recordset %s to become active: %w", rrID, err)
	}
	return nil
}

// waitForRecordSetDeleted blocks until the recordset is gone (404).
func waitForRecordSetDeleted(ctx context.Context, client *gophercloud.ServiceClient, zoneID, rrID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		_, err := recordsets.Get(ctx, client, zoneID, rrID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for recordset %s to delete: %w", rrID, err)
	}
	return nil
}
