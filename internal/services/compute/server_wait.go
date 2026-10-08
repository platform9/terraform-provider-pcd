// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
)

// serverPollInterval is a var so unit tests can shorten it.
var serverPollInterval = 5 * time.Second

// waitForServerSettled polls GET /servers/{id} until Nova has no task in
// flight (OS-EXT-STS:task_state empty) and the status is one of want. ERROR is
// fatal only once the task state is empty: Nova keeps status ERROR while it
// hard-reboots, rescues or rebuilds a VM that was in ERROR.
//
// A settled status outside want that is not ERROR keeps the wait going: after
// a confirmResize, for example, Nova reports VERIFY_RESIZE with no task while
// the source host cleans up, and only then the final status.
func waitForServerSettled(ctx context.Context, client *gophercloud.ServiceClient, id string, want []string, timeout time.Duration) (*servers.Server, error) {
	deadline := time.Now().Add(timeout)
	for {
		server, err := servers.Get(ctx, client, id).Extract()
		if err != nil {
			return nil, err
		}
		if server.TaskState == "" {
			if slices.Contains(want, server.Status) {
				return server, nil
			}
			if server.Status == "ERROR" {
				return nil, fmt.Errorf("instance %s entered ERROR state: %s", id, server.Fault.Message)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for instance %s to reach %s (last status %q, task state %q)",
				timeout, id, strings.Join(want, " or "), server.Status, server.TaskState)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(serverPollInterval):
		}
	}
}
