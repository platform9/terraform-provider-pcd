// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

// hostConfigRefusal is one way resmgr refuses a host configuration write, and
// the attribute that fixes it.
type hostConfigRefusal struct {
	code    int
	needle  string // substring of the resmgr message
	attr    string
	summary string
	hint    string
}

const (
	hostConfigInterfaceHint = "resmgr requires the management, VM console, host liveness, tunneling, and image library " +
		"interfaces when a configuration is created; set it to the host interface that carries that traffic."
	hostConfigInPlaceHint = "resmgr refuses this change on an existing configuration. Terraform plans a replacement when " +
		"it can see the change at plan time; a label value that was unknown until apply escapes that check, so apply " +
		"again with -replace for this resource."
	hostConfigImmutableHint = "resmgr does not allow this change on an existing configuration; changing this attribute " +
		"forces a new resource."
)

// hostConfigRefusals are matched in order against the resmgr message; the
// first match wins. The tunneling-label rule precedes the per-parameter rules
// because its message is itself phrased as a parameter. Every message here was
// observed on Community Edition 2026.4; a message this list does not know is
// left to the generic error, never sent to a guessed attribute.
var hostConfigRefusals = func() []hostConfigRefusal {
	rules := []hostConfigRefusal{
		{404, "ClusterBlueprintNotFound", "cluster_name", "cluster_name does not name a cluster blueprint",
			"cluster_name must be the name of the region's cluster blueprint (pcd_cluster_blueprint.<name>.name); " +
				"resmgr requires it when a configuration is created."},
		{400, "physical net label for tunneling interface", "network_labels", "network_labels has no entry for the tunneling interface",
			"The interface named by tunneling_interface must appear as a network_labels value. Add a label for it, " +
				"or point tunneling_interface at a labeled interface."},
		{400, "parameter: networkLabels ", "network_labels", "network_labels must map at least one label",
			"resmgr requires at least one physical-network label, and the tunneling interface must be one of the " +
				"values (for example physnet1 = <tunneling interface>)."},
		{400, "parameter: gpuPci ", "gpu_pci", "gpu_pci was rejected", ""},
		{409, "HostIntfConflict", "network_labels", "an interface carries at most one label",
			"Within a host configuration each interface can carry one physical-network label. Give each " +
				"network_labels entry its own interface."},
		{400, "Changing Network Label", "network_labels", "the interface an existing label maps to cannot be changed in place", hostConfigInPlaceHint},
		{400, "Changing Name", "name", "name cannot be changed in place", hostConfigImmutableHint},
		{400, "Changing Cluster Name", "cluster_name", "cluster_name cannot be changed in place", hostConfigImmutableHint},
	}
	for _, p := range []struct{ param, attr string }{
		{"mgmtInterface", "mgmt_interface"},
		{"vmConsoleInterface", "vm_console_interface"},
		{"hostLivenessInterface", "host_liveness_interface"},
		{"tunnelingInterface", "tunneling_interface"},
		{"imagelibInterface", "imagelib_interface"},
		{"liveMigrationInterface", "live_migration_interface"},
	} {
		rules = append(rules, hostConfigRefusal{400, "parameter: " + p.param + " ", p.attr, p.attr + " must be set", hostConfigInterfaceHint})
	}
	return rules
}()

// hostConfigAPIDiagnostic turns a refusal resmgr answered a host configuration
// write with into an error on the attribute that fixes it, carrying the resmgr
// message. op is the verb of the summary ("creating", "updating"). It returns
// nil for anything it does not recognize, and the caller reports the error as
// it always has.
func hostConfigAPIDiagnostic(op string, err error) diag.Diagnostic {
	var rc gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &rc) {
		return nil
	}
	msg := resmgrMessage(rc.Body)
	for _, r := range hostConfigRefusals {
		if rc.Actual != r.code || !strings.Contains(msg, r.needle) {
			continue
		}
		detail := fmt.Sprintf("resmgr answered %d: %s", rc.Actual, msg)
		if r.hint != "" {
			detail += "\n\n" + r.hint
		}
		return diag.NewAttributeErrorDiagnostic(path.Root(r.attr), fmt.Sprintf("resmgr: %s host config: %s", op, r.summary), detail)
	}
	return nil
}

// resmgrMessage returns the "message" of a resmgr error body, or the body
// itself when it is not shaped that way.
func resmgrMessage(body []byte) string {
	var b struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &b); err == nil && b.Message != "" {
		return b.Message
	}
	return strings.TrimSpace(string(body))
}
