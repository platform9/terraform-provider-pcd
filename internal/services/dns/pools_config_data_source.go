// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"gopkg.in/yaml.v3"
)

var _ datasource.DataSource = (*poolsConfigDataSource)(nil)

// NewPoolsConfigDataSource is the factory registered with the provider.
func NewPoolsConfigDataSource() datasource.DataSource {
	return &poolsConfigDataSource{}
}

// poolsConfigDataSource renders the pools.yaml that `designate-manage pool
// update` reads on a host carrying PCD's dns role. It never talks to an API:
// Designate's own pools API carries name, description, attributes and
// ns_records only, never targets or nameservers, so the file is the only
// complete description of a pool and the host is the only place it can go.
type poolsConfigDataSource struct{}

type poolsConfigModel struct {
	ID    types.String `tfsdk:"id"`
	Pools types.List   `tfsdk:"pools"`
	YAML  types.String `tfsdk:"yaml"`
}

type poolModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	Attributes   types.Map    `tfsdk:"attributes"`
	NSRecords    types.List   `tfsdk:"ns_records"`
	Nameservers  types.List   `tfsdk:"nameservers"`
	AlsoNotifies types.List   `tfsdk:"also_notifies"`
	Targets      types.List   `tfsdk:"targets"`
}

type nsRecordModel struct {
	Hostname types.String `tfsdk:"hostname"`
	Priority types.Int64  `tfsdk:"priority"`
}

type hostPortModel struct {
	Host types.String `tfsdk:"host"`
	Port types.Int64  `tfsdk:"port"`
}

type targetModel struct {
	Type        types.String `tfsdk:"type"`
	Description types.String `tfsdk:"description"`
	Masters     types.List   `tfsdk:"masters"`
	Options     types.Object `tfsdk:"options"`
}

type targetOptionsModel struct {
	Host           types.String `tfsdk:"host"`
	Port           types.Int64  `tfsdk:"port"`
	RNDCHost       types.String `tfsdk:"rndc_host"`
	RNDCPort       types.Int64  `tfsdk:"rndc_port"`
	RNDCKeyFile    types.String `tfsdk:"rndc_key_file"`
	RNDCConfigFile types.String `tfsdk:"rndc_config_file"`
	APIEndpoint    types.String `tfsdk:"api_endpoint"`
	APIToken       types.String `tfsdk:"api_token"`
}

// poolConfig is one pool as designate-manage reads it. Struct field order is
// the order the YAML is written in, which follows the upstream pools.yaml sample.
// designate-manage pool update parses the file onto the existing pool, so a key
// the file leaves out keeps its old value there. Description and also_notifies
// are therefore always written, empty when unset, so clearing either takes
// effect; id stays out when unset, because an empty id would be looked up.
type poolConfig struct {
	Name         string            `yaml:"name"`
	ID           string            `yaml:"id,omitempty"`
	Description  string            `yaml:"description"`
	Attributes   map[string]string `yaml:"attributes"`
	NSRecords    []nsRecord        `yaml:"ns_records"`
	Nameservers  []hostPort        `yaml:"nameservers"`
	AlsoNotifies []hostPort        `yaml:"also_notifies"`
	Targets      []poolTarget      `yaml:"targets"`
}

type nsRecord struct {
	Hostname string `yaml:"hostname"`
	Priority int    `yaml:"priority"`
}

type hostPort struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type poolTarget struct {
	Type        string        `yaml:"type"`
	Description string        `yaml:"description,omitempty"`
	Masters     []hostPort    `yaml:"masters"`
	Options     targetOptions `yaml:"options"`
}

type targetOptions struct {
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	RNDCHost       string `yaml:"rndc_host,omitempty"`
	RNDCPort       int    `yaml:"rndc_port,omitempty"`
	RNDCKeyFile    string `yaml:"rndc_key_file,omitempty"`
	RNDCConfigFile string `yaml:"rndc_config_file,omitempty"`
	APIEndpoint    string `yaml:"api_endpoint,omitempty"`
	APIToken       string `yaml:"api_token,omitempty"`
}

func (d *poolsConfigDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_pools_config"
}

func (d *poolsConfigDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	hostPortObject := func(hostDesc string) schema.NestedAttributeObject {
		return schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{Required: true, MarkdownDescription: hostDesc},
			"port": schema.Int64Attribute{Required: true, MarkdownDescription: "The port, 1 to 65535."},
		}}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Renders and validates the Designate `pools.yaml` for the hosts that carry PCD's `dns` cluster " +
			"role. PCD offers no API for a pool's targets and nameservers (Designate's pools API stops at name, " +
			"description, attributes and NS records), so the file still has to reach each DNS host and be applied " +
			"with `designate-manage pool update --file`. The Designate worker caches the pool, so restart " +
			"`pf9-designate-worker` on the DNS host afterward, as the DNS guide's example does; the guide shows one " +
			"way to deliver the file. What this data source adds is the typed schema and the checks that otherwise " +
			"need a page of variable validation: " +
			"NS record names end in a dot, hosts are IP literals, ports are in range, a target's options match its " +
			"type, and master addresses fit the width Designate can store for zones. The checks run when Terraform " +
			"reads the data source: at plan time when every input is known, and at apply time when an input comes " +
			"from a value computed in the same apply.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Sensitive: true,
				MarkdownDescription: "SHA-256 of `yaml`. It changes exactly when the rendered file changes, so it works as the " +
					"trigger for whatever delivers the file. Sensitive because it is an unsalted hash of `yaml`: anyone who " +
					"sees it and has the configuration could test guesses at a pdns4 `api_token` offline. It still works as " +
					"a trigger; plans show that it changed, not its value.",
			},
			"yaml": schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "The rendered `pools.yaml`: one document holding the list of pools. Sensitive because pdns4 targets carry an API token."},
			"pools": schema.ListNestedAttribute{
				Required:            true,
				MarkdownDescription: "The pools, in file order. Designate matches each by `id` when given, else by `name`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The pool name; `default` is the pool PCD creates. A pool's name cannot change after creation."},
					"id":          schema.StringAttribute{Optional: true, MarkdownDescription: "The UUID of an existing pool to update in place, as `GET /designate/v2/pools` reports it. Omit to match by name."},
					"description": schema.StringAttribute{Optional: true, MarkdownDescription: "A description of the pool."},
					"attributes":  schema.MapAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Pool attributes used for scheduling zones onto pools (for example `service_tier`)."},
					"ns_records": schema.ListNestedAttribute{
						Required:            true,
						MarkdownDescription: "The NS records that every zone hosted in this pool advertises. Their hostnames are names resolvable outside PCD that point at the pool's nameservers.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"hostname": schema.StringAttribute{Required: true, MarkdownDescription: "A fully qualified name ending in a dot."},
							"priority": schema.Int64Attribute{Required: true, MarkdownDescription: "The record's priority, 1 or greater."},
						}},
					},
					"nameservers": schema.ListNestedAttribute{
						Required:            true,
						MarkdownDescription: "The DNS servers Designate queries to confirm a change has propagated.",
						NestedObject:        hostPortObject("The nameserver's IP address."),
					},
					"also_notifies": schema.ListNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Extra servers that receive a NOTIFY on every zone change.",
						NestedObject:        hostPortObject("The server's IP address."),
					},
					"targets": schema.ListNestedAttribute{
						Required:            true,
						MarkdownDescription: "The backend servers Designate pushes zones to: one `bind9` target per BIND server, or a `pdns4` target per PowerDNS API endpoint.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"type":        schema.StringAttribute{Required: true, MarkdownDescription: "`bind9` or `pdns4`."},
							"description": schema.StringAttribute{Optional: true, MarkdownDescription: "A description of the target."},
							"masters": schema.ListNestedAttribute{
								Required:            true,
								MarkdownDescription: "The `designate-mdns` servers the backend transfers zones from: the address of each host carrying the `dns` role, port 5354. Keep each address at most 32 characters long: Designate copies them into every zone's masters, whose column is that wide (PCD-9946).",
								NestedObject:        hostPortObject("The mdns IP address."),
							},
							"options": schema.SingleNestedAttribute{
								Required:            true,
								MarkdownDescription: "How Designate reaches the backend. `host` and `port` always; `rndc_*` for `bind9`; `api_endpoint` and `api_token` for `pdns4`.",
								Attributes: map[string]schema.Attribute{
									"host":             schema.StringAttribute{Required: true, MarkdownDescription: "The backend's IP address."},
									"port":             schema.Int64Attribute{Required: true, MarkdownDescription: "The backend's DNS port, 1 to 65535."},
									"rndc_host":        schema.StringAttribute{Optional: true, MarkdownDescription: "bind9: the address rndc connects to. Designate defaults to `127.0.0.1`."},
									"rndc_port":        schema.Int64Attribute{Optional: true, MarkdownDescription: "bind9: the rndc control port. Designate defaults to 953."},
									"rndc_key_file":    schema.StringAttribute{Optional: true, MarkdownDescription: "bind9: the path, on the DNS host, of the rndc key file. One of `rndc_key_file` and `rndc_config_file` is required."},
									"rndc_config_file": schema.StringAttribute{Optional: true, MarkdownDescription: "bind9: the path, on the DNS host, of an rndc configuration file that carries the key."},
									"api_endpoint":     schema.StringAttribute{Optional: true, MarkdownDescription: "pdns4: the PowerDNS API URL."},
									"api_token":        schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "pdns4: the PowerDNS API key."},
								},
							},
						}},
					},
				}},
			},
		},
	}
}

func (d *poolsConfigDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data poolsConfigModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pools, diags := decodePools(ctx, data.Pools)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	errs, warns := validatePools(pools)
	for _, w := range warns {
		resp.Diagnostics.AddAttributeWarning(path.Root("pools"), "Pool configuration will fail on this PCD release", w.Path+": "+w.Msg)
	}
	for _, e := range errs {
		resp.Diagnostics.AddAttributeError(path.Root("pools"), "Invalid pool configuration", e.Path+": "+e.Msg)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := renderPoolsYAML(pools)
	if err != nil {
		resp.Diagnostics.AddError("dns: rendering pools.yaml", err.Error())
		return
	}
	sum := sha256.Sum256([]byte(out))
	data.YAML = types.StringValue(out)
	data.ID = types.StringValue(hex.EncodeToString(sum[:]))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// decodePools converts the framework values into plain pool configurations.
// Every level is decoded through the framework types so null optional blocks
// (also_notifies, rndc_* on a pdns4 target) come through as zero values.
func decodePools(ctx context.Context, list types.List) ([]poolConfig, diag.Diagnostics) {
	var diags diag.Diagnostics
	var pools []poolModel
	diags.Append(list.ElementsAs(ctx, &pools, false)...)
	if diags.HasError() {
		return nil, diags
	}
	out := make([]poolConfig, 0, len(pools))
	for _, p := range pools {
		pc := poolConfig{
			Name:        p.Name.ValueString(),
			ID:          p.ID.ValueString(),
			Description: p.Description.ValueString(),
			Attributes:  map[string]string{},
		}
		if !p.Attributes.IsNull() && !p.Attributes.IsUnknown() {
			diags.Append(p.Attributes.ElementsAs(ctx, &pc.Attributes, false)...)
		}
		var records []nsRecordModel
		diags.Append(p.NSRecords.ElementsAs(ctx, &records, false)...)
		for _, r := range records {
			pc.NSRecords = append(pc.NSRecords, nsRecord{Hostname: r.Hostname.ValueString(), Priority: int(r.Priority.ValueInt64())})
		}
		pc.Nameservers = hostPorts(ctx, p.Nameservers, &diags)
		pc.AlsoNotifies = hostPorts(ctx, p.AlsoNotifies, &diags)
		var targets []targetModel
		diags.Append(p.Targets.ElementsAs(ctx, &targets, false)...)
		for _, t := range targets {
			var o targetOptionsModel
			diags.Append(t.Options.As(ctx, &o, basetypes.ObjectAsOptions{})...)
			pc.Targets = append(pc.Targets, poolTarget{
				Type:        t.Type.ValueString(),
				Description: t.Description.ValueString(),
				Masters:     hostPorts(ctx, t.Masters, &diags),
				Options: targetOptions{
					Host:           o.Host.ValueString(),
					Port:           int(o.Port.ValueInt64()),
					RNDCHost:       o.RNDCHost.ValueString(),
					RNDCPort:       int(o.RNDCPort.ValueInt64()),
					RNDCKeyFile:    o.RNDCKeyFile.ValueString(),
					RNDCConfigFile: o.RNDCConfigFile.ValueString(),
					APIEndpoint:    o.APIEndpoint.ValueString(),
					APIToken:       o.APIToken.ValueString(),
				},
			})
		}
		out = append(out, pc)
	}
	return out, diags
}

func hostPorts(ctx context.Context, l types.List, diags *diag.Diagnostics) []hostPort {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var models []hostPortModel
	diags.Append(l.ElementsAs(ctx, &models, false)...)
	out := make([]hostPort, 0, len(models))
	for _, m := range models {
		out = append(out, hostPort{Host: m.Host.ValueString(), Port: int(m.Port.ValueInt64())})
	}
	return out
}

// poolIssue is one thing wrong with the configuration: where, and what.
type poolIssue struct {
	Path, Msg string
}

var poolTargetTypes = map[string]bool{"bind9": true, "pdns4": true}

// zoneMasterHostMax is the width of Designate's zone_masters.host column
// (String(32) upstream through at least 2024.1, the release PCD 2026.4 ships).
// designate-manage pool update copies every target master into every existing
// zone's masters, so a longer literal fails with "Data too long for column
// 'host'" (PCD-9946). The pool tables themselves are 255 wide, which is why
// the same file applies cleanly until the first zone exists.
const zoneMasterHostMax = 32

// validatePools checks what designate-manage would otherwise reject at apply
// time, or accept and then break on. Errors are malformed input; warnings are
// values that are valid YAML but known to fail on the PCD release this
// provider targets.
func validatePools(pools []poolConfig) (errs, warns []poolIssue) {
	if len(pools) == 0 {
		return []poolIssue{{"pools", "at least one pool is required"}}, nil
	}
	names, ids := map[string]int{}, map[string]int{}
	for i, p := range pools {
		at := fmt.Sprintf("pools[%d]", i)
		if p.Name == "" {
			errs = append(errs, poolIssue{at + ".name", "name is required"})
		} else if j, dup := names[p.Name]; dup {
			errs = append(errs, poolIssue{at + ".name", fmt.Sprintf("duplicates pools[%d].name %q; designate-manage matches pools by name", j, p.Name)})
		}
		names[p.Name] = i
		if p.ID != "" {
			if j, dup := ids[p.ID]; dup {
				errs = append(errs, poolIssue{at + ".id", fmt.Sprintf("duplicates pools[%d].id %q; designate-manage matches pools by id", j, p.ID)})
			}
			ids[p.ID] = i
		}
		if len(p.NSRecords) == 0 {
			errs = append(errs, poolIssue{at + ".ns_records", "at least one NS record is required; Designate refuses to create zones in a pool without one (no_servers_configured)"})
		}
		for j, r := range p.NSRecords {
			rat := fmt.Sprintf("%s.ns_records[%d]", at, j)
			if !strings.HasSuffix(r.Hostname, ".") || len(r.Hostname) < 2 {
				errs = append(errs, poolIssue{rat + ".hostname", fmt.Sprintf("%q must be a fully qualified name ending in a dot", r.Hostname)})
			}
			if r.Priority < 1 {
				errs = append(errs, poolIssue{rat + ".priority", "must be 1 or greater"})
			}
		}
		if len(p.Nameservers) == 0 {
			errs = append(errs, poolIssue{at + ".nameservers", "at least one nameserver is required"})
		}
		errs = append(errs, checkHostPorts(at+".nameservers", p.Nameservers)...)
		errs = append(errs, checkHostPorts(at+".also_notifies", p.AlsoNotifies)...)
		if len(p.Targets) == 0 {
			errs = append(errs, poolIssue{at + ".targets", "at least one target is required"})
		}
		for j, t := range p.Targets {
			tat := fmt.Sprintf("%s.targets[%d]", at, j)
			if !poolTargetTypes[t.Type] {
				errs = append(errs, poolIssue{tat + ".type", fmt.Sprintf("%q is not supported; use bind9 or pdns4", t.Type)})
			}
			if len(t.Masters) == 0 {
				errs = append(errs, poolIssue{tat + ".masters", "at least one master (the address of a host with the dns role, port 5354) is required"})
			}
			errs = append(errs, checkHostPorts(tat+".masters", t.Masters)...)
			for k, m := range t.Masters {
				if len(m.Host) > zoneMasterHostMax {
					warns = append(warns, poolIssue{fmt.Sprintf("%s.masters[%d].host", tat, k), longMasterWarning(m.Host)})
				}
			}
			o, oat := t.Options, tat+".options"
			if net.ParseIP(o.Host) == nil {
				errs = append(errs, poolIssue{oat + ".host", fmt.Sprintf("%q is not an IP address", o.Host)})
			}
			if !validPort(o.Port) {
				errs = append(errs, poolIssue{oat + ".port", "must be between 1 and 65535"})
			}
			switch t.Type {
			case "bind9":
				// Designate defaults rndc_host (127.0.0.1) and rndc_port (953) and
				// passes -k or -c to rndc only for the file options that are set.
				if o.RNDCKeyFile == "" && o.RNDCConfigFile == "" {
					errs = append(errs, poolIssue{oat, "a bind9 target needs rndc_key_file or rndc_config_file"})
				}
				if o.RNDCPort != 0 && !validPort(o.RNDCPort) {
					errs = append(errs, poolIssue{oat + ".rndc_port", "must be between 1 and 65535"})
				}
				if o.APIEndpoint != "" || o.APIToken != "" {
					errs = append(errs, poolIssue{oat, "api_endpoint and api_token are pdns4 options; a bind9 target does not take them"})
				}
			case "pdns4":
				if o.APIEndpoint == "" || o.APIToken == "" {
					errs = append(errs, poolIssue{oat, "a pdns4 target needs api_endpoint and api_token"})
				}
				if o.RNDCHost != "" || o.RNDCPort != 0 || o.RNDCKeyFile != "" || o.RNDCConfigFile != "" {
					errs = append(errs, poolIssue{oat, "rndc_host, rndc_port, rndc_key_file and rndc_config_file are bind9 options; a pdns4 target does not take them"})
				}
			}
		}
	}
	return errs, warns
}

// longMasterWarning explains PCD-9946 for a master address wider than the zone
// masters column. When the address is an IPv6 literal written out longer than
// it needs to be and its compressed form fits, the warning quotes that form.
func longMasterWarning(host string) string {
	msg := fmt.Sprintf("%q is %d characters, and Designate stores zone masters in a %d-character column: designate-manage pool update "+
		"fails with \"Data too long for column 'host'\" as soon as the pool has a zone (PCD-9946).", host, len(host), zoneMasterHostMax)
	// To4 guards the IPv4-mapped case, whose String() is an IPv4 address,
	// not a shorter spelling of the same IPv6 literal.
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		if short := ip.String(); len(short) <= zoneMasterHostMax {
			return msg + fmt.Sprintf(" Writing the address in its compressed form %q avoids the limit.", short)
		}
	}
	return msg + " Give designate-mdns a shorter static address, for example one with a compressible run of zeros."
}

func checkHostPorts(at string, hps []hostPort) []poolIssue {
	var errs []poolIssue
	for i, hp := range hps {
		if net.ParseIP(hp.Host) == nil {
			errs = append(errs, poolIssue{fmt.Sprintf("%s[%d].host", at, i), fmt.Sprintf("%q is not an IP address", hp.Host)})
		}
		if !validPort(hp.Port) {
			errs = append(errs, poolIssue{fmt.Sprintf("%s[%d].port", at, i), "must be between 1 and 65535"})
		}
	}
	return errs
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// renderPoolsYAML writes the pools the way designate-manage pool update reads
// them: one document holding a list of pools.
func renderPoolsYAML(pools []poolConfig) (string, error) {
	for i := range pools {
		if pools[i].Attributes == nil {
			pools[i].Attributes = map[string]string{}
		}
		if pools[i].AlsoNotifies == nil {
			pools[i].AlsoNotifies = []hostPort{}
		}
	}
	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(pools); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}
