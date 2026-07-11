// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"context"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
)

// TestLiveAuthenticateAndScope is a low-level smoke test that authenticates
// against a real PCD/OpenStack Keystone and reads back the token scope. It is
// skipped unless PCD_LIVE_TEST is set, so it never runs in unit-test CI. This
// verifies the gophercloud auth + TLS wiring independently of the Terraform
// acceptance harness.
//
//	PCD_LIVE_TEST=1 OS_AUTH_URL=... OS_USERNAME=... OS_PASSWORD=... \
//	OS_PROJECT_NAME=... OS_USER_DOMAIN_ID=... OS_PROJECT_DOMAIN_ID=... \
//	OS_REGION_NAME=... OS_INSECURE=true go test ./internal/clients/ -run Live -v
func TestLiveAuthenticateAndScope(t *testing.T) {
	if os.Getenv("PCD_LIVE_TEST") == "" {
		t.Skip("set PCD_LIVE_TEST=1 (plus OS_* env) to run the live auth smoke test")
	}

	cfg := &Config{
		AuthURL:         os.Getenv("OS_AUTH_URL"),
		Region:          os.Getenv("OS_REGION_NAME"),
		Username:        os.Getenv("OS_USERNAME"),
		Password:        os.Getenv("OS_PASSWORD"),
		TenantName:      os.Getenv("OS_PROJECT_NAME"),
		UserDomainID:    os.Getenv("OS_USER_DOMAIN_ID"),
		ProjectDomainID: os.Getenv("OS_PROJECT_DOMAIN_ID"),
		Insecure:        os.Getenv("OS_INSECURE") != "",
		AllowReauth:     true,
	}

	ctx := context.Background()
	if err := cfg.Authenticate(ctx); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	client, err := cfg.IdentityV3Client()
	if err != nil {
		t.Fatalf("IdentityV3Client: %v", err)
	}

	res := tokens.Get(ctx, client, client.Token())
	if res.Err != nil {
		t.Fatalf("tokens.Get: %v", res.Err)
	}

	user, err := res.ExtractUser()
	if err != nil {
		t.Fatalf("ExtractUser: %v", err)
	}
	if user.Name == "" {
		t.Fatal("expected a non-empty user name from the token scope")
	}
	t.Logf("user=%s id=%s user_domain=%s", user.Name, user.ID, user.Domain.ID)

	if project, err := res.ExtractProject(); err == nil && project != nil {
		t.Logf("project=%s id=%s project_domain=%s", project.Name, project.ID, project.Domain.ID)
	}

	roles, err := res.ExtractRoles()
	if err != nil {
		t.Fatalf("ExtractRoles: %v", err)
	}
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, r.Name)
	}
	t.Logf("roles=%v", names)
}
