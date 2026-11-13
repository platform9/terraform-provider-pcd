// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func userModelWithExtras(email, displayName types.String) *userModel {
	return &userModel{
		ID:               types.StringValue("u-1"),
		Name:             types.StringValue("alice@example.com"),
		Description:      types.StringValue(""),
		DomainID:         types.StringValue("default"),
		DefaultProjectID: types.StringValue(""),
		Enabled:          types.BoolValue(true),
		Password:         types.StringNull(),
		Email:            email,
		DisplayName:      displayName,
		Region:           types.StringValue("region-one"),
	}
}

// A new user carries each configured extra, "" included; an unset (null) or
// not-yet-known value is left to the server.
func TestUserCreateExtras(t *testing.T) {
	for _, tc := range []struct {
		name               string
		email, displayName types.String
		want               string
	}{
		{name: "both set", email: types.StringValue("a@example.com"), displayName: types.StringValue("Alice"),
			want: "map[displayname:Alice email:a@example.com]"},
		{name: "email only", email: types.StringValue("a@example.com"), displayName: types.StringNull(),
			want: "map[email:a@example.com]"},
		{name: "empty string is sent", email: types.StringValue(""), displayName: types.StringUnknown(),
			want: "map[email:]"},
		{name: "neither", email: types.StringNull(), displayName: types.StringUnknown(), want: "map[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := userCreateExtras(userModelWithExtras(tc.email, tc.displayName))
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("userCreateExtras = %v, want %s", got, tc.want)
			}
		})
	}
}

// An update sends an extra only when its planned value is known, not null and
// differs from the user's live value, so an unconfigured attribute never
// overwrites what the PCD UI wrote.
func TestUserUpdateExtras(t *testing.T) {
	live := &users.User{Extra: map[string]any{"email": "a@example.com", "displayname": "Alice", "username": "alice"}}
	for _, tc := range []struct {
		name               string
		email, displayName types.String
		live               *users.User
		want               string
	}{
		{name: "unchanged", email: types.StringValue("a@example.com"), displayName: types.StringValue("Alice"), live: live, want: "map[]"},
		{name: "email changed", email: types.StringValue("b@example.com"), displayName: types.StringValue("Alice"), live: live,
			want: "map[email:b@example.com]"},
		{name: "display name cleared", email: types.StringNull(), displayName: types.StringValue(""), live: live,
			want: "map[displayname:]"},
		{name: "unconfigured", email: types.StringUnknown(), displayName: types.StringNull(), live: live, want: "map[]"},
		{name: "user never had extras", email: types.StringValue("a@example.com"), displayName: types.StringNull(),
			live: &users.User{}, want: "map[email:a@example.com]"},
		{name: "live value is not a string", email: types.StringValue("1"), displayName: types.StringNull(),
			live: &users.User{Extra: map[string]any{"email": 1.0}}, want: "map[email:1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := userUpdateExtras(userModelWithExtras(tc.email, tc.displayName), tc.live)
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("userUpdateExtras = %v, want %s", got, tc.want)
			}
		})
	}
}

// Read reports what Keystone holds: null when the user has no such extra.
func TestUserExtraString(t *testing.T) {
	u := &users.User{Extra: map[string]any{"email": "a@example.com", "displayname": nil, "odd": 42.0, "blank": ""}}
	for key, want := range map[string]types.String{
		"email":       types.StringValue("a@example.com"),
		"displayname": types.StringNull(),
		"missing":     types.StringNull(),
		"odd":         types.StringValue("42"),
		"blank":       types.StringValue(""),
	} {
		if got := userExtraString(u, key); !got.Equal(want) {
			t.Errorf("userExtraString(%s) = %v, want %v", key, got, want)
		}
	}
}

// An update with email or display_name set reads the user first, sends only the
// extra that changed, and never sends options: pcd_identity_user_mfa owns the
// user's MFA options, and a PATCH carrying options would overwrite them.
func TestUserUpdateSendsChangedExtrasAndNoOptions(t *testing.T) {
	ctx := context.Background()
	const userJSON = `{"user": {"id": "u-1", "name": "alice@example.com", "domain_id": "default", "enabled": true,
		"description": "", "default_project_id": "", "options": {"multi_factor_auth_rules": [["password", "totp"]]},
		"email": "%s", "displayname": "Alice", "links": {}}}`
	f := newKeystoneFake(t, map[string]func() (int, string){
		"GET /v3/users/u-1":   keystoneReply(200, fmt.Sprintf(userJSON, "a@example.com")),
		"PATCH /v3/users/u-1": keystoneReply(200, fmt.Sprintf(userJSON, "b@example.com")),
	})
	r := &userResource{config: fakeConfig(f.srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, userModelWithExtras(types.StringValue("a@example.com"), types.StringValue("Alice"))); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, userModelWithExtras(types.StringValue("b@example.com"), types.StringUnknown())); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}

	body := f.bodyOf("PATCH /v3/users/u-1")["user"].(map[string]any)
	if body["email"] != "b@example.com" {
		t.Fatalf("PATCH email = %v, want b@example.com (body %v)", body["email"], body)
	}
	if _, sent := body["displayname"]; sent {
		t.Fatalf("PATCH sent displayname although it is not configured: %v", body)
	}
	if _, sent := body["options"]; sent {
		t.Fatalf("PATCH sent options, which would overwrite the user's MFA rules: %v", body)
	}
	var got userModel
	resp.State.Get(ctx, &got)
	if got.Email.ValueString() != "b@example.com" || got.DisplayName.ValueString() != "Alice" {
		t.Fatalf("state email=%s display_name=%s; want b@example.com, Alice", got.Email, got.DisplayName)
	}
	if want := []string{"GET /v3/users/u-1", "PATCH /v3/users/u-1"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", f.paths(), want)
	}
}

// An update with neither email nor display_name set has nothing to compare, so
// it does not read the user: it sends one PATCH, which carries neither extra.
func TestUserUpdateWithoutExtrasDoesNotReadTheUser(t *testing.T) {
	ctx := context.Background()
	f := newKeystoneFake(t, map[string]func() (int, string){
		"PATCH /v3/users/u-1": keystoneReply(200, `{"user": {"id": "u-1", "name": "alice@example.com", "domain_id": "default",
			"enabled": true, "description": "changed", "default_project_id": "", "email": "a@example.com", "links": {}}}`),
	})
	r := &userResource{config: fakeConfig(f.srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, userModelWithExtras(types.StringValue("a@example.com"), types.StringNull())); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	planned := userModelWithExtras(types.StringUnknown(), types.StringUnknown())
	planned.Description = types.StringValue("changed")
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if want := []string{"PATCH /v3/users/u-1"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", f.paths(), want)
	}
	body := f.bodyOf("PATCH /v3/users/u-1")["user"].(map[string]any)
	for _, key := range []string{"email", "displayname"} {
		if _, sent := body[key]; sent {
			t.Fatalf("PATCH sent %s although it is not configured: %v", key, body)
		}
	}
}

// A 200 without the user object decodes to nil. The read before an update
// reports it as an error, sends no PATCH, and keeps the prior state.
func TestUserUpdateRefusesAnAnswerWithoutTheUser(t *testing.T) {
	ctx := context.Background()
	f := newKeystoneFake(t, map[string]func() (int, string){"GET /v3/users/u-1": keystoneReply(200, `{}`)})
	r := &userResource{config: fakeConfig(f.srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, userModelWithExtras(types.StringValue("a@example.com"), types.StringNull())); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, userModelWithExtras(types.StringValue("b@example.com"), types.StringNull())); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update accepted an answer without the user")
	}
	if !resp.State.Raw.Equal(state.Raw) {
		t.Fatal("update changed the state although it failed")
	}
	if want := []string{"GET /v3/users/u-1"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", f.paths(), want)
	}
}
