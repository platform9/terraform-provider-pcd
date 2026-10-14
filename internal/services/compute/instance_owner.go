// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// changeInstanceOwner gives the instance to userID through PCD's changeOwner
// server action (admin only; synchronous, 202), the PCD UI's Change
// Ownership. It reads the live owner first and does nothing when it is
// already userID: Nova runs the new owner's per-user instance quota check even
// when the owner does not change, so a no-op call can fail with 403.
func changeInstanceOwner(ctx context.Context, client *gophercloud.ServiceClient, id, userID string) error {
	server, err := clients.RequireObject(servers.Get(ctx, client, id).Extract())
	if err == nil && server.ID == "" {
		err = clients.ErrNoObject
	}
	if err != nil {
		return err
	}
	if server.UserID == userID {
		return nil
	}
	_, err = client.Post(ctx, client.ServiceURL("servers", id, "action"),
		map[string]any{"changeOwner": map[string]string{"user_id": userID}}, nil,
		&gophercloud.RequestOpts{OkCodes: []int{http.StatusAccepted}})
	return err
}
