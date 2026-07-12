// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package acctest provides shared helpers for the provider's acceptance tests:
// the protocol-6 provider factory and a PreCheck that validates the lab
// environment and skips cleanly when it is absent.
package acctest

import (
	"context"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/provider"
)

// ProtoV6ProviderFactories wires the in-process provider under test for
// terraform-plugin-testing's resource.Test.
var ProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"pcd": providerserver.NewProtocol6WithError(provider.New("test")()),
}

// requiredEnv are the variables every acceptance test needs to reach a lab. The
// provider reads the full OS_* set as fallbacks; these three are the minimum
// that proves a target and credentials are present.
var requiredEnv = []string{"OS_AUTH_URL", "OS_USERNAME", "OS_PASSWORD"}

// PreCheck validates the environment and skips (not fails) when no lab is
// configured, so `go test ./...` stays green on a developer machine.
func PreCheck(t *testing.T) {
	t.Helper()
	var missing []string
	for _, e := range requiredEnv {
		if os.Getenv(e) == "" {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		t.Skipf("PCD acceptance tests require a reachable lab; missing env: %v", missing)
	}
}

// LabConfig returns an authenticated client built from the OS_* environment, for
// use in CheckDestroy/CheckExists helpers that query the API out of band.
func LabConfig(t *testing.T) *clients.Config {
	t.Helper()
	cfg := &clients.Config{
		AuthURL:         os.Getenv("OS_AUTH_URL"),
		Region:          os.Getenv("OS_REGION_NAME"),
		Username:        os.Getenv("OS_USERNAME"),
		Password:        os.Getenv("OS_PASSWORD"),
		TenantName:      firstEnv("OS_PROJECT_NAME", "OS_TENANT_NAME"),
		TenantID:        firstEnv("OS_PROJECT_ID", "OS_TENANT_ID"),
		UserDomainID:    os.Getenv("OS_USER_DOMAIN_ID"),
		ProjectDomainID: os.Getenv("OS_PROJECT_DOMAIN_ID"),
		Insecure:        os.Getenv("OS_INSECURE") != "",
		AllowReauth:     true,
	}
	if err := cfg.Authenticate(context.Background()); err != nil {
		t.Fatalf("acctest: authenticate to lab: %v", err)
	}
	return cfg
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
