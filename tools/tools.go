// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build tools

// Package tools pins developer tool dependencies (documentation generation) so
// they are versioned with the module. It is never built into the provider.
package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
)
