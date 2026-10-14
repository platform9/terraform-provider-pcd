// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/instanceactions"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework/action"
)

// Microversions the migrations need, each set on one call's client copy.
const (
	// computeMicroversionLiveMigrateAuto takes block_migration "auto", which
	// the PCD UI always sends, and from 2.68 a named host is never forced:
	// the scheduler checks it like any other.
	computeMicroversionLiveMigrateAuto = "2.68"
	// computeMicroversionColdMigrateHost lets a cold migration name its target.
	computeMicroversionColdMigrateHost = "2.56"
	// computeMicroversionMigrateAZ adds availability_zone to a cold migration,
	// which the PCD UI sends to move a VM to another cluster.
	computeMicroversionMigrateAZ = "2.101"
)

// migrateTimeout bounds each wait of one migration: the wait for the instance
// to settle after the request, and the wait after its confirm, so a confirmed
// cold migration can take up to twice this. A var so unit tests can shorten it.
var migrateTimeout = 30 * time.Minute

// settledStatuses are the steady statuses a migrating server can end in:
// where it started (success of a live migration, or failure of either kind),
// or VERIFY_RESIZE after a cold migration. confirmedStatuses drop the last.
var (
	settledStatuses   = []string{"ACTIVE", "PAUSED", "SHUTOFF", "SUSPENDED", "VERIFY_RESIZE"}
	confirmedStatuses = []string{"ACTIVE", "PAUSED", "SHUTOFF", "SUSPENDED"}
)

// liveMigratable and coldMigratable are the statuses Nova accepts for each
// kind. PCD's Nova also cold-migrates SUSPENDED instances, which upstream
// Nova does not.
func liveMigratable(status string) bool { return status == "ACTIVE" || status == "PAUSED" }
func coldMigratable(status string) bool {
	return status == "ACTIVE" || status == "SHUTOFF" || status == "SUSPENDED"
}

// liveMigrateOpts sends the body the PCD UI sends. gophercloud's
// LiveMigrateOpts holds block_migration as a *bool and so cannot say "auto",
// which lets Nova pick block or shared-storage migration per instance; from
// 2.25 the field is required. host nil lets the scheduler choose.
type liveMigrateOpts struct {
	Host string
}

func (o liveMigrateOpts) ToLiveMigrateMap() (map[string]any, error) {
	var host any
	if o.Host != "" {
		host = o.Host
	}
	return map[string]any{"os-migrateLive": map[string]any{"host": host, "block_migration": "auto"}}, nil
}

// coldMigrateBody is the migrate action body for a cold migration to a
// chosen host, optionally in another availability zone (another cluster).
func coldMigrateBody(host, zone string) map[string]any {
	args := map[string]any{"host": host}
	if zone != "" {
		args["availability_zone"] = zone
	}
	return map[string]any{"migrate": args}
}

// migrateRequest says how to move one instance. Host "" lets the scheduler
// choose; AvailabilityZone needs Host and a cold migration.
type migrateRequest struct {
	Live             bool
	Host             string
	AvailabilityZone string
	Confirm          bool
}

// migrateResult is what a migration that did not fail left behind.
type migrateResult struct {
	Server   *servers.Server
	FromHost string
	// Unconfirmed is set when Confirm was false and the instance waits in
	// VERIFY_RESIZE for someone to confirm or revert the cold migration.
	Unconfirmed bool
}

// migrateServer moves srv (read just before, by an admin, so its host is
// known), waits for Nova to finish, confirms a cold migration when asked, and
// checks the instance really left its host: Nova answers 202 to a migration
// that later fails (NoValidHost, for one) and leaves the instance where it
// was, so a settled status alone proves nothing. progress reports each phase
// and, through settleWithProgress, a heartbeat while Nova works.
func migrateServer(ctx context.Context, client *gophercloud.ServiceClient, srv *servers.Server, req migrateRequest, progress func(string)) (*migrateResult, error) {
	if srv.Host == "" {
		return nil, errNotAdmin
	}
	from := srv.Host
	kind := "cold"
	if req.Live {
		kind = "live"
	}
	requestID, err := postMigration(ctx, client, srv.ID, req)
	if err != nil {
		return nil, err
	}
	progress(fmt.Sprintf("%s migration of %s off %s requested; waiting for Nova", kind, srv.ID, from))

	heartbeat := func(e action.InvokeProgressEvent) { progress(e.Message) }
	server, err := settleWithProgress(ctx, client, srv.ID, settledStatuses, migrateTimeout, heartbeat)
	if err != nil {
		return nil, fmt.Errorf("waiting for the %s migration of %s: %w", kind, srv.ID, err)
	}
	if server.Status == "VERIFY_RESIZE" {
		if !req.Confirm {
			return &migrateResult{Server: server, FromHost: from, Unconfirmed: true}, nil
		}
		progress(fmt.Sprintf("confirming the cold migration of %s to %s", srv.ID, server.Host))
		if err := servers.ConfirmResize(ctx, client, srv.ID).ExtractErr(); err != nil {
			return nil, fmt.Errorf("confirming the cold migration of %s: %w", srv.ID, err)
		}
		if server, err = settleWithProgress(ctx, client, srv.ID, confirmedStatuses, migrateTimeout, heartbeat); err != nil {
			return nil, fmt.Errorf("waiting for the confirmed migration of %s: %w", srv.ID, err)
		}
	}
	if server.Host == from {
		return nil, fmt.Errorf("instance %s is still on %s after its %s migration: %s",
			srv.ID, from, kind, migrationFailureDetail(ctx, client, srv.ID, requestID))
	}
	progress(fmt.Sprintf("%s moved from %s to %s", srv.ID, from, server.Host))
	return &migrateResult{Server: server, FromHost: from}, nil
}

// postMigration sends the migration and returns Nova's request ID, which
// names the instance action that records how it went.
func postMigration(ctx context.Context, client *gophercloud.ServiceClient, id string, req migrateRequest) (string, error) {
	switch {
	case req.Live:
		res := servers.LiveMigrate(ctx, withMicroversion(client, computeMicroversionLiveMigrateAuto), id, liveMigrateOpts{Host: req.Host})
		if err := res.ExtractErr(); err != nil {
			return "", fmt.Errorf("live-migrating %s: %w", id, err)
		}
		return res.Header.Get("X-Openstack-Request-Id"), nil
	case req.Host == "":
		res := servers.Migrate(ctx, client, id)
		if err := res.ExtractErr(); err != nil {
			return "", fmt.Errorf("cold-migrating %s: %w", id, err)
		}
		return res.Header.Get("X-Openstack-Request-Id"), nil
	default:
		mv := computeMicroversionColdMigrateHost
		if req.AvailabilityZone != "" {
			mv = computeMicroversionMigrateAZ
		}
		c := withMicroversion(client, mv)
		resp, err := c.Post(ctx, c.ServiceURL("servers", id, "action"), coldMigrateBody(req.Host, req.AvailabilityZone), nil,
			&gophercloud.RequestOpts{OkCodes: []int{http.StatusAccepted}})
		if err != nil {
			if req.AvailabilityZone != "" && gophercloud.ResponseCodeIs(err, http.StatusNotAcceptable) {
				return "", fmt.Errorf("cold-migrating %s: this PCD does not support migration to another cluster "+
					"(compute microversion %s): %w", id, computeMicroversionMigrateAZ, err)
			}
			return "", fmt.Errorf("cold-migrating %s: %w", id, err)
		}
		return resp.Header.Get("X-Openstack-Request-Id"), nil
	}
}

// migrationFailureDetail explains a migration that left the instance in place,
// from the instance action Nova recorded for the request: the last line of
// each failed event's traceback (an admin sees tracebacks at any microversion).
func migrationFailureDetail(ctx context.Context, client *gophercloud.ServiceClient, serverID, requestID string) string {
	const fallback = "Nova reported no error; read the instance's action log with `pcdctl server event list`"
	if requestID == "" {
		return fallback
	}
	ia, err := instanceactions.Get(ctx, client, serverID, requestID).Extract()
	if err != nil {
		return fmt.Sprintf("%s (reading instance action %s failed: %v)", fallback, requestID, err)
	}
	var details []string
	if ia.Events != nil {
		for _, ev := range *ia.Events {
			if ev.Result != "Error" {
				continue
			}
			details = append(details, ev.Event+": "+lastLine(ev.Traceback))
		}
	}
	if len(details) == 0 {
		if ia.Message != "" {
			return ia.Message
		}
		return fallback
	}
	return strings.Join(details, "; ")
}

// lastLine is the final non-empty line of a Python traceback: the exception.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// errNotAdmin is returned when the caller cannot see where an instance runs.
var errNotAdmin = errors.New("the instance's host is not visible to this token; migrating needs an admin token " +
	"(by default Nova shows OS-EXT-SRV-ATTR:host only to admins)")
