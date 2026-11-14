// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// changeTenantConfirmTimeout bounds the reads that confirm a move. Nova moves
// the instance before it answers, so the first read normally shows the new
// project. A var so unit tests can shorten it.
var changeTenantConfirmTimeout = 2 * time.Minute

// changeTenantIDShape is the shape of an instance ID that goes into a URL
// path. Nova's instance IDs are UUIDs, which it matches.
var changeTenantIDShape = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// errInstanceIDMismatch marks an answer for another instance than the one
// asked for.
var errInstanceIDMismatch = errors.New("the answer is for another instance")

// checkInstanceIDShape refuses an instance ID that cannot go into a URL path
// as one segment, before any request is sent.
func checkInstanceIDShape(id string) error {
	if !changeTenantIDShape.MatchString(id) {
		return fmt.Errorf("instance_id %q is not an instance ID: it must be one or more letters, digits, '-' or '_'", id)
	}
	return nil
}

// readInstanceForMove reads the instance and checks that the answer is the
// instance asked for: a 200 without the server, or with another server's ID,
// is an error. An admin token reads an instance in any project.
func readInstanceForMove(ctx context.Context, client *gophercloud.ServiceClient, id string) (*servers.Server, error) {
	if err := checkInstanceIDShape(id); err != nil {
		return nil, err
	}
	server, err := clients.RequireObject(servers.Get(ctx, client, id).Extract())
	if err != nil {
		return nil, err
	}
	// servers.Get decodes a 200 without the server to an empty server, not to
	// nil, so RequireObject passes it.
	if server.ID == "" {
		return nil, clients.ErrNoObject
	}
	if server.ID != id {
		if strings.EqualFold(server.ID, id) {
			return nil, fmt.Errorf("%w: asked for instance %s, Nova answered for %s; Nova answers in lower case, "+
				"so set instance_id to the instance's ID as Nova returns it", errInstanceIDMismatch, id, server.ID)
		}
		return nil, fmt.Errorf("%w: asked for instance %s, Nova answered for %s", errInstanceIDMismatch, id, server.ID)
	}
	return server, nil
}

// moveInstanceToProject moves the instance to projectID through PCD's
// changeTenant server action (admin only), as the PCD UI's Change Tenant does:
// the body {"changeTenant": {"project_id": ...}} with no microversion header.
// Nova checks everything and moves the instance, its ports and its attached
// volumes before it answers 202, and rolls the ports and volumes back if a
// step fails. A refusal (400, 403, 404 or 409) comes back as an error that
// carries Nova's message.
func moveInstanceToProject(ctx context.Context, client *gophercloud.ServiceClient, id, projectID string) error {
	if err := checkInstanceIDShape(id); err != nil {
		return err
	}
	_, err := client.Post(ctx, client.ServiceURL("servers", id, "action"),
		map[string]any{"changeTenant": map[string]string{"project_id": projectID}}, nil,
		&gophercloud.RequestOpts{OkCodes: []int{http.StatusAccepted}})
	return err
}

// waitForInstanceProject reads the instance until Nova reports it in
// projectID, and returns it. A malformed ID is refused before the first
// request. progress is called before each pause between reads.
func waitForInstanceProject(ctx context.Context, client *gophercloud.ServiceClient, id, projectID string, progress func(string)) (*servers.Server, error) {
	if err := checkInstanceIDShape(id); err != nil {
		return nil, err
	}
	// The timeout is on the context, so it also ends a request that Nova never
	// answers: the HTTP client has no timeout of its own.
	ctx, cancel := context.WithTimeout(ctx, changeTenantConfirmTimeout)
	defer cancel()
	last := ""
	// The wrapped ctx.Err() tells a deadline from a cancel of the caller's
	// context.
	timedOut := func() error {
		if errors.Is(ctx.Err(), context.Canceled) {
			return fmt.Errorf("the wait for instance %s to report project %s was interrupted after Nova accepted the move: %w",
				id, projectID, ctx.Err())
		}
		if last == "" {
			return fmt.Errorf("instance %s could not be read within %s after Nova accepted the move: %w",
				id, changeTenantConfirmTimeout, ctx.Err())
		}
		return fmt.Errorf("instance %s still reports project %s, not %s, %s after Nova accepted the move: %w",
			id, last, projectID, changeTenantConfirmTimeout, ctx.Err())
	}
	for {
		server, err := readInstanceForMove(ctx, client, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, timedOut()
			}
			return nil, fmt.Errorf("instance %s could not be read after Nova accepted the move: %w", id, err)
		}
		if server.TenantID == projectID {
			return server, nil
		}
		last = server.TenantID
		if server.Status == "ERROR" {
			if server.Fault.Message == "" {
				return nil, fmt.Errorf("instance %s is in ERROR in project %s", id, server.TenantID)
			}
			return nil, fmt.Errorf("instance %s is in ERROR in project %s: %s", id, server.TenantID, server.Fault.Message)
		}
		progress(fmt.Sprintf("Instance %s still reports project %s; reading it again", id, server.TenantID))
		select {
		case <-ctx.Done():
			return nil, timedOut()
		case <-time.After(serverPollInterval):
		}
	}
}

// changeTenantRefusalHint says what Nova checks for the status it refused a
// move with. Nova's own message stays in the error it goes with.
func changeTenantRefusalHint(err error) string {
	switch {
	case gophercloud.ResponseCodeIs(err, http.StatusBadRequest) && strings.Contains(err.Error(), "There is no such action"):
		return "This PCD release's Nova does not have the changeTenant action, so it cannot move an instance to " +
			"another project; PCD 2026.8's Nova has it."
	case gophercloud.ResponseCodeIs(err, http.StatusBadRequest):
		return "Before it moves anything, Nova checks that the target project differs from the instance's " +
			"project; that the instance's owner may create instances in the target project; that the instance is " +
			"in no server group and has no floating IP; that every security group on its ports belongs to the " +
			"target project (security groups do not move); that the target project can use its image, flavor, " +
			"networks and volume types; and that no attached volume has snapshots, is encrypted, or is anything " +
			"but available or in-use."
	case gophercloud.ResponseCodeIs(err, http.StatusConflict):
		return "The instance must be active, shut off, paused or suspended, with no task in progress, and not locked."
	case gophercloud.ResponseCodeIs(err, http.StatusForbidden):
		return "Moving an instance needs an admin token, and the target project needs instance quota left for " +
			"the instance's owner."
	case gophercloud.ResponseCodeIs(err, http.StatusNotFound):
		return "Nova found no such instance, or no such project."
	}
	return ""
}
