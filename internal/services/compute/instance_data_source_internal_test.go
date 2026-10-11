// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// instanceDSServerJSON renders a Nova server record at microversion 2.26. The security
// group is listed twice, once per port, as Nova does.
func instanceDSServerJSON(id, name, status, image string) string {
	imageField := `""`
	if image != "" {
		imageField = fmt.Sprintf(`{"id": %q}`, image)
	}
	return fmt.Sprintf(`{"id": %q, "name": %q, "status": %q, "tenant_id": "p-1", "user_id": "u-1",
		"OS-EXT-STS:vm_state": "active", "OS-EXT-AZ:availability_zone": "nova",
		"OS-EXT-SRV-ATTR:host": "host-uuid-1", "OS-EXT-SRV-ATTR:hypervisor_hostname": "compute-01",
		"key_name": "kp", "image": %s, "flavor": {"id": "flv-1"},
		"accessIPv4": "", "accessIPv6": "",
		"metadata": {"env": "dev", "migration-priority": "high"},
		"tags": ["web"],
		"security_groups": [{"name": "default"}, {"name": "default"}, {"name": "web"}],
		"addresses": {
			"app-net": [{"addr": "10.0.0.5", "version": 4, "OS-EXT-IPS-MAC:mac_addr": "fa:16:3e:00:00:02"}],
			"mgmt-net": [{"addr": "192.168.1.9", "version": 4, "OS-EXT-IPS-MAC:mac_addr": "fa:16:3e:00:00:01"},
			             {"addr": "fd00::9", "version": 6, "OS-EXT-IPS-MAC:mac_addr": "fa:16:3e:00:00:01"}]
		},
		"created": "2026-10-06T10:00:00Z", "updated": "2026-10-06T10:05:00Z"}`, id, name, status, imageField)
}

// The ports come back newest first, as Neutron may list them; the data source
// must order them oldest first.
const instanceDSPortsJSON = `{"ports": [
	{"id": "port-b", "network_id": "net-app", "mac_address": "fa:16:3e:00:00:02", "device_id": "srv-1",
	 "created_at": "2026-10-06T10:00:02Z", "fixed_ips": [{"subnet_id": "s-a", "ip_address": "10.0.0.5"}]},
	{"id": "port-a", "network_id": "net-mgmt", "mac_address": "fa:16:3e:00:00:01", "device_id": "srv-1",
	 "created_at": "2026-10-06T10:00:01Z", "fixed_ips": [{"subnet_id": "s-6", "ip_address": "fd00::9"},
	                                                     {"subnet_id": "s-m", "ip_address": "192.168.1.9"}]}
]}`

// instanceDSAPI fakes Nova, Neutron and Glance behind one endpoint (their paths
// do not collide: Nova has no version prefix, Neutron uses /v2.0, Glance /v2).
// The tests reach it through the package's shared fakeConfig
// (fake_nova_internal_test.go), which gives each client a transport of its own.
type instanceDSAPI struct {
	mu         sync.Mutex
	server     string // GET /servers/srv-1
	list       string // GET /servers/detail
	listStatus int    // status for GET /servers/detail; 0 answers list
	flavor     int    // status for GET /flavors/flv-1
	flavorBody string // body for GET /flavors/flv-1 when flavor is 0; "" is m1.small
	image      int    // status for GET /v2/images/img-1
	requests   []*http.Request
	versions   map[string]string // path -> X-OpenStack-Nova-API-Version
	listQuery  url.Values
}

func (a *instanceDSAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	a.versions = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests = append(a.requests, r)
		a.versions[r.URL.Path] = r.Header.Get("X-OpenStack-Nova-API-Version")
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /servers/srv-1":
			if a.server == "" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"itemNotFound": {"code": 404}}`)
				return
			}
			fmt.Fprintf(w, `{"server": %s}`, a.server)
		case "GET /servers/detail":
			a.listQuery = r.URL.Query()
			if a.listStatus != 0 {
				w.WriteHeader(a.listStatus)
				fmt.Fprint(w, `{"forbidden": {"code": 403}}`)
				return
			}
			fmt.Fprintf(w, `{"servers": [%s]}`, a.list)
		case "GET /flavors/flv-1":
			if a.flavor != 0 {
				w.WriteHeader(a.flavor)
				fmt.Fprint(w, `{"itemNotFound": {"code": 404}}`)
				return
			}
			if a.flavorBody != "" {
				fmt.Fprint(w, a.flavorBody)
				return
			}
			fmt.Fprint(w, `{"flavor": {"id": "flv-1", "name": "m1.small"}}`)
		case "GET /v2/images/img-1":
			if a.image != 0 {
				w.WriteHeader(a.image)
				return
			}
			fmt.Fprint(w, `{"id": "img-1", "name": "ubuntu-24.04", "status": "active"}`)
		case "GET /v2.0/ports":
			if got := r.URL.Query().Get("device_id"); got != "srv-1" {
				t.Errorf("ports listed with device_id=%q, want srv-1", got)
			}
			fmt.Fprint(w, instanceDSPortsJSON)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// version returns the compute microversion the last request to path carried.
func (a *instanceDSAPI) version(path string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.versions[path]
}

// query returns the query of the last server list.
func (a *instanceDSAPI) query() url.Values {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listQuery
}

func (a *instanceDSAPI) called(method, path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.requests {
		if r.Method == method && r.URL.Path == path {
			return true
		}
	}
	return false
}

// instanceDSRead runs Read with the given configuration values (others null).
func instanceDSRead(ctx context.Context, t *testing.T, d *instanceDataSource, set map[string]string) (datasource.ReadResponse, instanceDataSourceModel) {
	t.Helper()
	var sch datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &sch)
	s := sch.Schema
	typ := s.Type().TerraformType(ctx).(tftypes.Object)
	vals := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := set[name]; ok {
			vals[name] = tftypes.NewValue(tftypes.String, v)
		} else {
			vals[name] = tftypes.NewValue(at, nil)
		}
	}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(typ, nil)}}
	d.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: tftypes.NewValue(typ, vals)}}, &resp)
	var m instanceDataSourceModel
	if !resp.Diagnostics.HasError() {
		if diags := resp.State.Get(ctx, &m); diags.HasError() {
			t.Fatalf("reading state: %v", diags)
		}
	}
	return resp, m
}

func instanceDSNetworkAt(ctx context.Context, t *testing.T, m instanceDataSourceModel, i int) instanceDataSourceNetworkModel {
	t.Helper()
	var nets []instanceDataSourceNetworkModel
	if d := m.Network.ElementsAs(ctx, &nets, false); d.HasError() {
		t.Fatal(d)
	}
	if i >= len(nets) {
		t.Fatalf("network has %d entries, want index %d", len(nets), i)
	}
	return nets[i]
}

// A lookup by ID reads Nova at 2.26 and fills every attribute: the network
// list comes from Neutron in creation order with names from Nova's addresses,
// the access addresses follow that order, the doubled security group appears
// once, and the migration priority is split out of the metadata.
func TestInstanceDataSourceByID(t *testing.T) {
	ctx := context.Background()
	api := &instanceDSAPI{server: instanceDSServerJSON("srv-1", "web", "SUSPENDED", "img-1")}
	srv := api.serve(t)
	d := &instanceDataSource{config: fakeConfig(srv.URL)}

	resp, m := instanceDSRead(ctx, t, d, map[string]string{"instance_id": "srv-1"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if v := api.version("/servers/srv-1"); v != computeMicroversionServerTags {
		t.Fatalf("GET /servers/srv-1 sent microversion %q, want %q (tags need 2.26)", v, computeMicroversionServerTags)
	}
	for name, got := range map[string]string{
		"id": m.ID.ValueString(), "instance_id": m.InstanceID.ValueString(), "name": m.Name.ValueString(),
		"project_id": m.ProjectID.ValueString(), "user_id": m.UserID.ValueString(),
		"status": m.Status.ValueString(), "vm_state": m.VMState.ValueString(),
		"image_id": m.ImageID.ValueString(), "image_name": m.ImageName.ValueString(),
		"flavor_id": m.FlavorID.ValueString(), "flavor_name": m.FlavorName.ValueString(),
		"key_pair": m.KeyPair.ValueString(), "availability_zone": m.AvailabilityZone.ValueString(),
		"access_ip_v4": m.AccessIPv4.ValueString(), "access_ip_v6": m.AccessIPv6.ValueString(),
		"migration_priority": m.MigrationPriority.ValueString(), "host": m.Host.ValueString(),
		"hypervisor_hostname": m.HypervisorHostname.ValueString(), "created": m.Created.ValueString(),
		"region": m.Region.ValueString(),
	} {
		want := map[string]string{
			"id": "srv-1", "instance_id": "srv-1", "name": "web", "project_id": "p-1", "user_id": "u-1",
			"status": "SUSPENDED", "vm_state": "active", "image_id": "img-1", "image_name": "ubuntu-24.04",
			"flavor_id": "flv-1", "flavor_name": "m1.small", "key_pair": "kp", "availability_zone": "nova",
			// port-a (mgmt-net) is older, so its addresses come first.
			"access_ip_v4": "192.168.1.9", "access_ip_v6": "fd00::9",
			"migration_priority": "high", "host": "host-uuid-1", "hypervisor_hostname": "compute-01",
			"created": "2026-10-06T10:00:00Z", "region": "region-one",
		}[name]
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if n := len(m.SecurityGroups.Elements()); n != 2 {
		t.Errorf("security_groups has %d entries, want 2 (default once, web)", n)
	}
	if _, leaked := m.Metadata.Elements()["migration-priority"]; leaked || len(m.Metadata.Elements()) != 1 {
		t.Errorf("metadata = %v, want only env", m.Metadata)
	}
	if n := len(m.Tags.Elements()); n != 1 {
		t.Errorf("tags has %d entries, want 1", n)
	}
	first, second := instanceDSNetworkAt(ctx, t, m, 0), instanceDSNetworkAt(ctx, t, m, 1)
	if first.Port.ValueString() != "port-a" || first.UUID.ValueString() != "net-mgmt" || first.Name.ValueString() != "mgmt-net" ||
		first.FixedIPv4.ValueString() != "192.168.1.9" || first.FixedIPv6.ValueString() != "fd00::9" {
		t.Errorf("network[0] = %+v, want port-a on net-mgmt (mgmt-net) with 192.168.1.9 and fd00::9", first)
	}
	if second.Port.ValueString() != "port-b" || second.Name.ValueString() != "app-net" || second.FixedIPv6.ValueString() != "" {
		t.Errorf("network[1] = %+v, want port-b on app-net with no IPv6", second)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("state holds unknown values: %v", resp.State.Raw)
	}
}

// A lookup by name filters exactly, so "web-2" and "WEB" (Nova's regex filter
// can return both) do not count, and the name goes out anchored.
func TestInstanceDataSourceByName(t *testing.T) {
	ctx := context.Background()
	api := &instanceDSAPI{
		list: instanceDSServerJSON("srv-1", "web", "ACTIVE", "img-1") + "," + instanceDSServerJSON("srv-2", "web-2", "ACTIVE", "img-1") +
			"," + instanceDSServerJSON("srv-3", "WEB", "ACTIVE", "img-1"),
	}
	srv := api.serve(t)
	d := &instanceDataSource{config: fakeConfig(srv.URL)}

	resp, m := instanceDSRead(ctx, t, d, map[string]string{"name": "web"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if m.ID.ValueString() != "srv-1" {
		t.Fatalf("id = %s, want srv-1", m.ID)
	}
	if got := api.query().Get("name"); got != "^web$" {
		t.Fatalf("name filter = %q, want ^web$", got)
	}
	if api.query().Has("all_tenants") {
		t.Fatalf("listed all projects without project_id: %v", api.query())
	}
}

// A name with regular-expression characters is matched here, not sent: as a
// pattern, "web(1)" would not match the instance named "web(1)".
func TestInstanceDataSourceByNameWithRegexCharacters(t *testing.T) {
	ctx := context.Background()
	api := &instanceDSAPI{list: instanceDSServerJSON("srv-1", "web(1)", "ACTIVE", "img-1") + "," + instanceDSServerJSON("srv-2", "web1", "ACTIVE", "img-1")}
	srv := api.serve(t)
	d := &instanceDataSource{config: fakeConfig(srv.URL)}

	resp, m := instanceDSRead(ctx, t, d, map[string]string{"name": "web(1)"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if m.ID.ValueString() != "srv-1" {
		t.Fatalf("id = %s, want srv-1", m.ID)
	}
	if api.query().Has("name") {
		t.Fatalf("sent name=%q as a pattern; it cannot match itself", api.query().Get("name"))
	}
}

// project_id scopes a name search to another project (admin).
func TestInstanceDataSourceProjectScope(t *testing.T) {
	ctx := context.Background()
	api := &instanceDSAPI{list: instanceDSServerJSON("srv-1", "web", "ACTIVE", "img-1")}
	srv := api.serve(t)
	d := &instanceDataSource{config: fakeConfig(srv.URL)}

	resp, _ := instanceDSRead(ctx, t, d, map[string]string{"name": "web", "project_id": "p-1"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if api.query().Get("all_tenants") != "true" || api.query().Get("tenant_id") != "p-1" {
		t.Fatalf("query = %v, want all_tenants=true and tenant_id=p-1", api.query())
	}
}

// Lookup errors: no key, no match, several matches, a missing ID, an ID whose
// instance has another name, an ID in another project, and a 200 whose body
// holds no server or no flavor object (not a not-found, and never a crash).
func TestInstanceDataSourceLookupErrors(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		api         *instanceDSAPI
		set         map[string]string
		wantSummary string
		wantDetail  []string // substrings the error's detail must hold
	}{
		{name: "no key", api: &instanceDSAPI{}, set: map[string]string{}, wantSummary: "Missing lookup key"},
		{name: "no match", api: &instanceDSAPI{list: instanceDSServerJSON("srv-2", "web-2", "ACTIVE", "")}, set: map[string]string{"name": "web"}, wantSummary: "No instance found"},
		{name: "two matches", api: &instanceDSAPI{list: instanceDSServerJSON("srv-1", "web", "ACTIVE", "") + "," + instanceDSServerJSON("srv-2", "web", "ACTIVE", "")}, set: map[string]string{"name": "web"}, wantSummary: "Multiple instances found"},
		{name: "missing id", api: &instanceDSAPI{}, set: map[string]string{"instance_id": "srv-1"}, wantSummary: "No instance found"},
		{name: "name mismatch", api: &instanceDSAPI{server: instanceDSServerJSON("srv-1", "web", "ACTIVE", "")}, set: map[string]string{"instance_id": "srv-1", "name": "db"}, wantSummary: "Instance name does not match"},
		{name: "other project", api: &instanceDSAPI{server: instanceDSServerJSON("srv-1", "web", "ACTIVE", "")}, set: map[string]string{"instance_id": "srv-1", "project_id": "p-9"}, wantSummary: "Instance is in another project"},
		{name: "answer without the server", api: &instanceDSAPI{server: "null"}, set: map[string]string{"instance_id": "srv-1"}, wantSummary: "compute: reading instance"},
		{name: "answer without the flavor", api: &instanceDSAPI{server: instanceDSServerJSON("srv-1", "web", "ACTIVE", ""), flavorBody: "{}"}, set: map[string]string{"instance_id": "srv-1"}, wantSummary: "compute: reading the instance's flavor"},
		{name: "project filter forbidden", api: &instanceDSAPI{listStatus: http.StatusForbidden}, set: map[string]string{"name": "web", "project_id": "p-9"}, wantSummary: "compute: listing instances", wantDetail: []string{"403", "`project_id` needs the admin role"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.api.serve(t)
			d := &instanceDataSource{config: fakeConfig(srv.URL)}
			resp, _ := instanceDSRead(ctx, t, d, tc.set)
			if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != tc.wantSummary {
				t.Fatalf("diagnostics = %v, want an error %q", resp.Diagnostics, tc.wantSummary)
			}
			for _, want := range tc.wantDetail {
				if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, want) {
					t.Errorf("detail = %q, want it to contain %q", detail, want)
				}
			}
		})
	}
}

// A boot-from-volume instance has no image and Glance is not asked; a deleted
// flavor reads as ""; a Glance failure leaves image_name "" without a warning
// (it would repeat on every plan where Glance is unreachable).
func TestInstanceDataSourceBestEffortNames(t *testing.T) {
	ctx := context.Background()

	api := &instanceDSAPI{server: instanceDSServerJSON("srv-1", "web", "ACTIVE", ""), flavor: http.StatusNotFound}
	srv := api.serve(t)
	resp, m := instanceDSRead(ctx, t, &instanceDataSource{config: fakeConfig(srv.URL)}, map[string]string{"instance_id": "srv-1"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if m.ImageID.ValueString() != "" || m.ImageName.ValueString() != "" || m.FlavorName.ValueString() != "" {
		t.Fatalf("image_id=%q image_name=%q flavor_name=%q, want all empty", m.ImageID.ValueString(), m.ImageName.ValueString(), m.FlavorName.ValueString())
	}
	if api.called(http.MethodGet, "/v2/images/") {
		t.Fatal("asked Glance for the image of a boot-from-volume instance")
	}

	api = &instanceDSAPI{server: instanceDSServerJSON("srv-1", "web", "ACTIVE", "img-1"), image: http.StatusInternalServerError}
	srv = api.serve(t)
	resp, m = instanceDSRead(ctx, t, &instanceDataSource{config: fakeConfig(srv.URL)}, map[string]string{"instance_id": "srv-1"})
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 0 {
		t.Fatalf("diagnostics = %v, want none when Glance fails", resp.Diagnostics)
	}
	if m.ImageID.ValueString() != "img-1" || m.ImageName.ValueString() != "" {
		t.Fatalf("image_id=%q image_name=%q, want img-1 and empty", m.ImageID.ValueString(), m.ImageName.ValueString())
	}
}

// Ports with equal creation times fall back to port ID order, so the list is
// stable whatever order Neutron returns.
func TestInstanceNICsOrder(t *testing.T) {
	at := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	nics := instanceNICs(nil, []ports.Port{
		{ID: "port-c", NetworkID: "n3", CreatedAt: at},
		{ID: "port-a", NetworkID: "n1", CreatedAt: at},
		{ID: "port-b", NetworkID: "n2", CreatedAt: at.Add(-time.Second)},
	})
	var got []string
	for _, n := range nics {
		got = append(got, n.PortID)
	}
	if fmt.Sprint(got) != "[port-b port-a port-c]" {
		t.Fatalf("order = %v, want [port-b port-a port-c]", got)
	}
}

func TestInstanceAccessIPsPreferServerFields(t *testing.T) {
	v4, v6 := instanceAccessIPs(&servers.Server{AccessIPv4: "203.0.113.5"}, []instanceNIC{{FixedIPv4: "10.0.0.5", FixedIPv6: "fd00::5"}})
	if v4 != "203.0.113.5" || v6 != "fd00::5" {
		t.Fatalf("access ips = %s, %s; want 203.0.113.5 (accessIPv4) and fd00::5 (first fixed)", v4, v6)
	}
}

func TestNameRegexSafe(t *testing.T) {
	for name, want := range map[string]bool{
		"web": true, "web-1": true, "web_1": true, "web.example.com": true, "my vm": true,
		"web(1)": false, "a|b": false, "web*": false, "[x]": false, `back\slash`: false, "^web": false,
	} {
		if got := nameRegexSafe(name); got != want {
			t.Errorf("nameRegexSafe(%q) = %v, want %v", name, got, want)
		}
	}
}
