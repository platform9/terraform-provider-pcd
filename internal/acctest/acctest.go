// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package acctest provides shared helpers for the provider's acceptance tests:
// the protocol-6 provider factory and a PreCheck that validates the lab
// environment and skips cleanly when it is absent.
package acctest

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

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
