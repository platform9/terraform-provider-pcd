// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
)

// powerStates are the values of pcd_compute_instance.power_state: the steady
// states the PCD UI's Power Actions menu reaches.
var powerStates = []string{"active", "shutoff", "paused", "suspended"}

// powerStateFromStatus maps a Nova status to a power_state value. Any other
// status (BUILD, REBOOT, RESIZE, VERIFY_RESIZE, MIGRATING, RESCUE, ERROR,
// SHELVED, ...) is not a power state, and ok is false.
func powerStateFromStatus(status string) (state string, ok bool) {
	switch status {
	case "ACTIVE":
		return "active", true
	case "SHUTOFF":
		return "shutoff", true
	case "PAUSED":
		return "paused", true
	case "SUSPENDED":
		return "suspended", true
	}
	return "", false
}

// powerTransitionTimeout bounds each step of a power transition.
const powerTransitionTimeout = 30 * time.Minute

// setPowerState moves an instance whose Nova status is status to the
// power_state target and returns the status it ends in. Every route goes
// through ACTIVE, because Nova starts only a stopped instance, unpauses only a
// paused one, resumes only a suspended one, and stops, pauses or suspends only
// an active one. Each step waits until Nova has no task in flight. A step
// that Nova accepts and then fails ends with the instance back in the status
// it started from and no task, so each wait also stops there and reports it.
func setPowerState(ctx context.Context, client *gophercloud.ServiceClient, id, status, target string) (string, error) {
	from, ok := powerStateFromStatus(status)
	if !ok {
		return status, fmt.Errorf("instance %s is %s; power_state can change only from ACTIVE, SHUTOFF, PAUSED or "+
			"SUSPENDED. Wait for the current operation to finish, or recover an instance in ERROR with a hard reboot "+
			"(the pcd_compute_instance_reboot action), and apply again", id, status)
	}
	if from == target {
		return status, nil
	}
	if from != "active" {
		var err error
		switch from {
		case "shutoff":
			err = servers.Start(ctx, client, id).ExtractErr()
		case "paused":
			err = servers.Unpause(ctx, client, id).ExtractErr()
		case "suspended":
			err = servers.Resume(ctx, client, id).ExtractErr()
		}
		if err != nil {
			return status, fmt.Errorf("powering on instance %s from %s: %w", id, status, err)
		}
		settled, err := waitForServerSettled(ctx, client, id, []string{"ACTIVE", status}, powerTransitionTimeout)
		if err != nil {
			return status, err
		}
		if settled.Status != "ACTIVE" {
			return settled.Status, powerStepUndone(id, settled.Status, "ACTIVE")
		}
		status = "ACTIVE"
	}
	var want string
	var err error
	switch target {
	case "active":
		return status, nil
	case "shutoff":
		want = "SHUTOFF"
		err = servers.Stop(ctx, client, id).ExtractErr()
	case "paused":
		want = "PAUSED"
		err = servers.Pause(ctx, client, id).ExtractErr()
	case "suspended":
		want = "SUSPENDED"
		err = servers.Suspend(ctx, client, id).ExtractErr()
	default:
		return status, fmt.Errorf("unknown power_state %q", target)
	}
	if err != nil {
		return status, fmt.Errorf("setting instance %s to %s: %w", id, target, err)
	}
	settled, err := waitForServerSettled(ctx, client, id, []string{want, status}, powerTransitionTimeout)
	if err != nil {
		return status, err
	}
	if settled.Status != want {
		return settled.Status, powerStepUndone(id, settled.Status, want)
	}
	return want, nil
}

// powerStepUndone reports a power step that Nova accepted and then failed.
func powerStepUndone(id, status, want string) error {
	return fmt.Errorf("instance %s is back in %s instead of %s: Nova accepted the request and then failed it. "+
		"The instance's action log (openstack server event list %s) has the reason", id, status, want, id)
}
