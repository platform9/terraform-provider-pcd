// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const credentialBody = `{"credential": {"id": "c-1", "type": "totp", "user_id": "u-1", "blob": "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", "links": {}}}`

func mfaUserBody(options string) string {
	return `{"user": {"id": "u-1", "name": "alice", "domain_id": "default", "enabled": true, "options": ` + options + `, "links": {}}}`
}

// A generated secret is 160 random bits in the unpadded, upper-case base32
// Keystone decodes, and the validator accepts it. The validator rejects what
// Keystone cannot use: lower case, other characters, a key shorter than 128
// bits, and a length or padding that does not decode.
func TestGenerateTOTPSecret(t *testing.T) {
	a, err := generateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generateTOTPSecret()
	if len(a) != 32 || !totpSecretPattern.MatchString(a) || a == b {
		t.Fatalf("secrets %q and %q; want two different 32-character base32 strings", a, b)
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(a)
	if err != nil || len(raw) != 20 {
		t.Fatalf("decoding %q: %d bytes, %v; want 20 bytes", a, len(raw), err)
	}
	for _, bad := range []string{
		"",
		"jbswy3dpehpk3pxpjbswy3dpehpk3pxp",     // lower case
		"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PX1",     // 1 is not base32
		"JBSWY3DPEHPK3PXP",                     // 80 bits
		"JBSWY3DPEHPK3PXPJBSWY3DP",             // 120 bits
		"JBSWY3DPEHPK3PXPJBSWY3DPE",            // 25 characters do not decode
		"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP====", // padding after a full block
		"JBSWY3DPEHPK3PXPJBSWY3DPEH=======",    // too much padding
	} {
		if totpSecretPattern.MatchString(bad) {
			t.Errorf("totpSecretPattern accepts %q, which Keystone cannot use", bad)
		}
	}
	for _, good := range []string{
		"JBSWY3DPEHPK3PXPJBSWY3DPEH",       // 128 bits, the PCD UI's length
		"JBSWY3DPEHPK3PXPJBSWY3DPEH======", // the same, padded
		"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PX",  // 152 bits
		"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", // 160 bits
	} {
		if !totpSecretPattern.MatchString(good) {
			t.Errorf("totpSecretPattern rejects %q, which Keystone can use", good)
		}
	}
}

// MFA is enforced only when every rule needs totp and the rules are not
// switched off; the PCD UI's "disabled" value [["password"]] is not enforced.
func TestMFAEnforced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options map[string]any
		want    bool
	}{
		{name: "UI enabled", options: map[string]any{"multi_factor_auth_rules": []any{[]any{"password", "totp"}}}, want: true},
		{name: "flag true", options: map[string]any{"multi_factor_auth_rules": []any{[]any{"password", "totp"}}, "multi_factor_auth_enabled": true}, want: true},
		{name: "flag false", options: map[string]any{"multi_factor_auth_rules": []any{[]any{"password", "totp"}}, "multi_factor_auth_enabled": false}},
		{name: "UI disabled", options: map[string]any{"multi_factor_auth_rules": []any{[]any{"password"}}}},
		{name: "totp optional", options: map[string]any{"multi_factor_auth_rules": []any{[]any{"password"}, []any{"password", "totp"}}}},
		{name: "no rules", options: map[string]any{}},
		{name: "nil options", options: nil},
	} {
		if got := mfaEnforced(tc.options); got != tc.want {
			t.Errorf("%s: mfaEnforced = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Enabling enrolls the credential first, then sets only the user's MFA options.
func TestEnableMFAEnrollsThenSetsRules(t *testing.T) {
	f := newKeystoneFake(t, map[string]func() (int, string){
		"POST /credentials": keystoneReply(201, credentialBody),
		"PATCH /users/u-1":  keystoneReply(200, mfaUserBody(`{"multi_factor_auth_rules": [["password", "totp"]]}`)),
	})
	id, err := enableMFA(context.Background(), f.bareClient(), "u-1", "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP")
	if err != nil || id != "c-1" {
		t.Fatalf("enableMFA = %q, %v; want c-1", id, err)
	}
	if want := []string{"POST /credentials", "PATCH /users/u-1"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", f.paths(), want)
	}
	if got := fmt.Sprint(f.bodyOf("POST /credentials")); got != "map[credential:map[blob:JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP type:totp user_id:u-1]]" {
		t.Fatalf("credential body = %s", got)
	}
	if got := fmt.Sprint(f.bodyOf("PATCH /users/u-1")); got != "map[user:map[options:map[multi_factor_auth_enabled:true multi_factor_auth_rules:[[password totp]]]]]" {
		t.Fatalf("user PATCH body = %s; want only the MFA options", got)
	}
}

// When Keystone refuses the rules (a 4xx), the credential is deleted again, so
// no half-enabled MFA is left behind. Any other failure may hide rules Keystone
// did set, so the user is read back first: the credential is deleted only when
// the rules are not in effect, and kept when they are or the read fails, since
// deleting it then would leave the user unable to log in. The credential ID is
// returned whenever the credential is kept or its delete fails, for the caller
// to record.
func TestEnableMFARollsBackTheCredential(t *testing.T) {
	const (
		post, patch, get, del = "POST /credentials", "PATCH /users/u-1", "GET /users/u-1", "DELETE /credentials/c-1"
		enforced              = `{"multi_factor_auth_rules": [["password", "totp"]]}`
	)
	for _, tc := range []struct {
		name      string
		patch     int
		routes    map[string]func() (int, string)
		wantID    string
		wantErr   string
		wantCalls []string
	}{
		{name: "refused, rolled back", patch: 403, routes: map[string]func() (int, string){del: keystoneReply(204, "")},
			wantErr: "deleted again", wantCalls: []string{post, patch, del}},
		{name: "refused, already gone", patch: 403, routes: map[string]func() (int, string){del: keystoneReply(404, "")},
			wantErr: "deleted again", wantCalls: []string{post, patch, del}},
		{name: "refused, rollback failed", patch: 403, routes: map[string]func() (int, string){del: keystoneReply(500, "")},
			wantID: "c-1", wantErr: "also failed", wantCalls: []string{post, patch, del}},
		{name: "answer lost, rules not in effect", patch: 502, routes: map[string]func() (int, string){
			get: keystoneReply(200, mfaUserBody(`{}`)), del: keystoneReply(204, ""),
		}, wantErr: "deleted again", wantCalls: []string{post, patch, get, del}},
		{name: "answer lost, rules in effect", patch: 502, routes: map[string]func() (int, string){
			get: keystoneReply(200, mfaUserBody(enforced)),
		}, wantID: "c-1", wantErr: "was kept", wantCalls: []string{post, patch, get}},
		{name: "answer lost, user unreadable", patch: 502, routes: map[string]func() (int, string){
			get: keystoneReply(500, `{"error": {"message": "boom"}}`),
		}, wantID: "c-1", wantErr: "was kept", wantCalls: []string{post, patch, get}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := map[string]func() (int, string){
				post:  keystoneReply(201, credentialBody),
				patch: keystoneReply(tc.patch, `{"error": {"message": "boom"}}`),
			}
			for k, v := range tc.routes {
				routes[k] = v
			}
			f := newKeystoneFake(t, routes)
			id, err := enableMFA(context.Background(), f.bareClient(), "u-1", "JBSWY3DPEHPK3PXP")
			if id != tc.wantID || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("enableMFA = %q, %v; want %q and an error containing %q", id, err, tc.wantID, tc.wantErr)
			}
			if fmt.Sprint(f.paths()) != fmt.Sprint(tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", f.paths(), tc.wantCalls)
			}
		})
	}
}

// Disabling removes the options (sent as null, which Keystone deletes) before
// the credential, and treats 404s as already done.
func TestDisableMFARemovesRulesThenCredential(t *testing.T) {
	for _, tc := range []struct {
		name       string
		patch, del int
		wantErr    string
	}{
		{name: "both removed", patch: 200, del: 204},
		{name: "user gone", patch: 404, del: 404},
		{name: "credential gone", patch: 200, del: 404},
		{name: "rules refused", patch: 403, del: 204, wantErr: "removing the MFA rules"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeystoneFake(t, map[string]func() (int, string){
				"PATCH /users/u-1":        keystoneReply(tc.patch, mfaUserBody(`{}`)),
				"DELETE /credentials/c-1": keystoneReply(tc.del, ""),
			})
			err := disableMFA(context.Background(), f.bareClient(), "u-1", "c-1")
			if tc.wantErr == "" && err != nil {
				t.Fatalf("disableMFA: %v", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("disableMFA error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if want := []string{"PATCH /users/u-1", "DELETE /credentials/c-1"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
				t.Fatalf("calls = %v, want %v", f.paths(), want)
			}
			if got := fmt.Sprint(f.bodyOf("PATCH /users/u-1")); got != "map[user:map[options:map[multi_factor_auth_enabled:<nil> multi_factor_auth_rules:<nil>]]]" {
				t.Fatalf("user PATCH body = %s; want both MFA options sent as null", got)
			}
		})
	}
}

func mfaSchemaState(t *testing.T, r *userMFAResource) (tfsdk.Plan, tfsdk.State) {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	return tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)},
		tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
}

// Create, a refresh that finds the rules changed outside Terraform, the
// update that restores them, and destroy, against a fake Keystone.
func TestUserMFAResourceLifecycle(t *testing.T) {
	ctx := context.Background()
	rules := `{"multi_factor_auth_rules": [["password", "totp"]]}`
	f := newKeystoneFake(t, map[string]func() (int, string){
		"GET /v3/auth/tokens":        keystoneReply(200, tokenBody),
		"POST /v3/credentials":       keystoneReply(201, credentialBody),
		"PATCH /v3/users/u-1":        keystoneReply(200, mfaUserBody(rules)),
		"GET /v3/credentials/c-1":    keystoneReply(200, credentialBody),
		"GET /v3/users/u-1":          func() (int, string) { return 200, mfaUserBody(rules) },
		"DELETE /v3/credentials/c-1": keystoneReply(204, ""),
		mfaListKey:                   keystoneReply(200, totpList("c-1")),
	})
	r := &userMFAResource{config: fakeConfig(f.srv.URL)}
	plan, empty := mfaSchemaState(t, r)
	if d := plan.Set(ctx, &userMFAModel{
		ID: types.StringUnknown(), UserID: types.StringValue("u-1"), TOTPSecret: types.StringUnknown(),
		CredentialID: types.StringUnknown(), Enforced: types.BoolValue(true), Region: types.StringUnknown(),
	}); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	createResp := resource.CreateResponse{State: empty}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	var got userMFAModel
	createResp.State.Get(ctx, &got)
	if got.ID.ValueString() != "u-1" || got.CredentialID.ValueString() != "c-1" || !got.Enforced.ValueBool() ||
		!totpSecretPattern.MatchString(got.TOTPSecret.ValueString()) {
		t.Fatalf("create state = %+v; want id u-1, credential c-1, enforced, a generated secret", got)
	}
	generated := f.bodyOf("POST /v3/credentials")["credential"].(map[string]any)["blob"]
	if generated != got.TOTPSecret.ValueString() {
		t.Fatalf("enrolled secret %v differs from the one in state %s", generated, got.TOTPSecret)
	}

	// Someone sets the PCD UI's "disabled" rules: refresh reports enforced = false
	// and the secret Keystone holds.
	rules = `{"multi_factor_auth_rules": [["password"]]}`
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	readResp.State.Get(ctx, &got)
	if got.Enforced.ValueBool() || got.TOTPSecret.ValueString() != "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP" {
		t.Fatalf("refreshed enforced=%v secret=%s; want false and the credential's blob", got.Enforced, got.TOTPSecret)
	}

	// The plan always wants enforced = true, so the next apply restores the rules.
	rules = `{"multi_factor_auth_rules": [["password", "totp"]]}`
	planned := got
	planned.Enforced = types.BoolValue(true)
	updatePlan, _ := mfaSchemaState(t, r)
	updatePlan.Set(ctx, &planned)
	updateResp := resource.UpdateResponse{State: readResp.State}
	r.Update(ctx, resource.UpdateRequest{Plan: updatePlan, State: readResp.State}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updateResp.Diagnostics)
	}
	// The update's PATCH, the last request so far, restores exactly what
	// enabling sets: the rules and multi_factor_auth_enabled = true.
	f.mu.Lock()
	restore := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	var restoreBody map[string]any
	if err := json.Unmarshal([]byte(restore.body), &restoreBody); err != nil || restore.path != "PATCH /v3/users/u-1" ||
		fmt.Sprint(restoreBody) != "map[user:map[options:map[multi_factor_auth_enabled:true multi_factor_auth_rules:[[password totp]]]]]" {
		t.Fatalf("update sent %s %s; want a PATCH of only the MFA rules and multi_factor_auth_enabled = true", restore.path, restore.body)
	}

	deleteResp := resource.DeleteResponse{State: updateResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: updateResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	want := []string{
		"GET /v3/auth/tokens", "POST /v3/credentials", "PATCH /v3/users/u-1", // create
		"GET /v3/credentials/c-1", "GET /v3/users/u-1", // refresh
		"GET /v3/auth/tokens", "PATCH /v3/users/u-1", // update checks the user, then restores the rules
		mfaListKey, "PATCH /v3/users/u-1", "DELETE /v3/credentials/c-1", // destroy: no other credential, so MFA goes off
	}
	if fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v\nwant    %v", f.paths(), want)
	}
}

// mfaListKey is the request that lists user u-1's TOTP credentials.
const mfaListKey = "GET /v3/credentials?type=totp&user_id=u-1"

// totpList is a credential listing answer holding TOTP credentials of user u-1
// with the given IDs.
func totpList(ids ...string) string {
	var items []string
	for _, id := range ids {
		items = append(items, `{"id": "`+id+`", "type": "totp", "user_id": "u-1", "blob": "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", "links": {}}`)
	}
	return `{"credentials": [` + strings.Join(items, ", ") + `], "links": {"next": null}}`
}

// mfaState builds the state of an enforced pcd_identity_user_mfa for u-1 whose
// credential is credentialID.
func mfaState(t *testing.T, r *userMFAResource, credentialID string) tfsdk.State {
	t.Helper()
	_, state := mfaSchemaState(t, r)
	if d := state.Set(context.Background(), &userMFAModel{
		ID: types.StringValue("u-1"), UserID: types.StringValue("u-1"),
		TOTPSecret: types.StringValue("JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"), CredentialID: types.StringValue(credentialID),
		Enforced: types.BoolValue(true), Region: types.StringValue("region-one"),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return state
}

// replySequence answers with the given replies in turn, repeating the last.
// The fake's handlers run on the server's goroutines, hence the mutex.
func replySequence(replies ...func() (int, string)) func() (int, string) {
	var mu sync.Mutex
	n := 0
	return func() (int, string) {
		mu.Lock()
		defer mu.Unlock()
		reply := replies[min(n, len(replies)-1)]
		n++
		return reply()
	}
}

// Destroy turns MFA off only when the user has no TOTP credential other than
// the resource's own. While another remains, a secret the user enrolled or the
// new credential of a create-before-destroy replacement, it deletes only its
// own credential, lists again, keeps the rules and warns. When the second
// listing finds none left, as when the current and the deposed object of one
// user are destroyed at once, it clears the rules instead. A failed listing
// returns the error and sends no PATCH.
func TestUserMFADeleteKeepsMFAWhileAnotherTOTPCredentialRemains(t *testing.T) {
	ctx := context.Background()
	const patch, del = "PATCH /v3/users/u-1", "DELETE /v3/credentials/c-1"
	listFails := keystoneReply(500, `{"error": {"message": "boom"}}`)
	for _, tc := range []struct {
		name        string
		list        func() (int, string)
		wantErr     string
		wantWarning string // a substring of the warning's detail; "" for no warning
		wantAdvice  bool   // the warning gives the recovery advice, not the old "delete that credential"
		wantRerun   bool   // the error says to run again with -refresh=false, naming the credential
		wantCalls   []string
	}{
		{name: "no other credential", list: keystoneReply(200, totpList("c-1")),
			wantCalls: []string{mfaListKey, patch, del}},
		{name: "own credential already gone", list: keystoneReply(200, totpList()),
			wantCalls: []string{mfaListKey, patch, del}},
		{name: "a self-enrolled credential remains", list: keystoneReply(200, totpList("c-1", "c-self")),
			wantWarning: "User u-1 still has 1 TOTP credential(s) (c-self), so destroy deleted only TOTP credential c-1",
			wantAdvice:  true, wantCalls: []string{mfaListKey, del, mfaListKey}},
		{name: "two others remain", list: replySequence(keystoneReply(200, totpList("c-z", "c-1", "c-a")),
			keystoneReply(200, totpList("c-z", "c-a"))),
			wantWarning: "User u-1 still has 2 TOTP credential(s) (c-a, c-z)",
			wantCalls:   []string{mfaListKey, del, mfaListKey}},
		{name: "the other credential is gone after the delete", list: replySequence(
			keystoneReply(200, totpList("c-1", "c-other")), keystoneReply(200, totpList())),
			wantCalls: []string{mfaListKey, del, mfaListKey, patch}},
		{name: "listing fails", list: listFails,
			wantErr: "identity: listing TOTP credentials", wantCalls: []string{mfaListKey}},
		{name: "second listing fails", list: replySequence(keystoneReply(200, totpList("c-1", "c-other")), listFails),
			wantErr: "identity: listing TOTP credentials", wantRerun: true, wantCalls: []string{mfaListKey, del, mfaListKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeystoneFake(t, map[string]func() (int, string){
				mfaListKey: tc.list,
				patch:      keystoneReply(200, mfaUserBody(`{}`)),
				del:        keystoneReply(204, ""),
			})
			r := &userMFAResource{config: fakeConfig(f.srv.URL)}
			state := mfaState(t, r, "c-1")
			resp := resource.DeleteResponse{State: state}
			r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
			if tc.wantErr == "" && resp.Diagnostics.HasError() {
				t.Fatalf("delete: %v", resp.Diagnostics)
			}
			if tc.wantErr != "" && (!resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != tc.wantErr) {
				t.Fatalf("delete diagnostics = %v; want the error %q", resp.Diagnostics, tc.wantErr)
			}
			if tc.wantRerun {
				errs := resp.Diagnostics.Errors()
				if len(errs) != 1 || !strings.Contains(errs[0].Detail(), "-refresh=false") ||
					!strings.Contains(errs[0].Detail(), "c-1") || strings.Contains(errs[0].Detail(), "destroy again to finish") {
					t.Fatalf("delete diagnostics = %v; want one error naming c-1 that says to run again with -refresh=false", resp.Diagnostics)
				}
				if resp.State.Raw.IsNull() {
					t.Fatal("the resource was removed from state")
				}
			}
			warnings := resp.Diagnostics.Warnings()
			if (len(warnings) > 0) != (tc.wantWarning != "") {
				t.Fatalf("warnings = %v; want a warning: %v", warnings, tc.wantWarning != "")
			}
			if tc.wantWarning != "" && (len(warnings) != 1 || warnings[0].Summary() != "MFA rules left in place" ||
				!strings.Contains(warnings[0].Detail(), tc.wantWarning)) {
				t.Fatalf("warnings = %v; want one \"MFA rules left in place\" containing %q", warnings, tc.wantWarning)
			}
			if tc.wantAdvice && (!strings.Contains(warnings[0].Detail(), "Deleting a credential alone leaves the rules in place") ||
				strings.Contains(warnings[0].Detail(), "delete that credential")) {
				t.Fatalf("warning detail = %q; want the advice that deleting a credential alone leaves the rules", warnings[0].Detail())
			}
			for _, d := range resp.Diagnostics {
				if strings.Contains(d.Summary()+d.Detail(), "JBSWY3DP") {
					t.Fatalf("a diagnostic shows a TOTP secret: %q", d.Summary())
				}
			}
			if fmt.Sprint(f.paths()) != fmt.Sprint(tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", f.paths(), tc.wantCalls)
			}
			// Turning MFA off sends both options as null.
			if slices.Contains(tc.wantCalls, patch) {
				if got := fmt.Sprint(f.bodyOf(patch)); got != "map[user:map[options:map[multi_factor_auth_enabled:<nil> multi_factor_auth_rules:<nil>]]]" {
					t.Fatalf("PATCH body = %s; want both MFA options null", got)
				}
			}
		})
	}
}

// When the second listing finds no other credential left but removing the
// rules then fails, the own credential is already gone. The error says so and
// that running again with -refresh=false finishes the job, and the resource
// stays in state. A refresh of that state finds the credential gone and drops
// the resource, which is why a plain retry would not reach Delete.
func TestUserMFADeleteSaysWhatToDoWhenTheLastClearFails(t *testing.T) {
	ctx := context.Background()
	const patch, del = "PATCH /v3/users/u-1", "DELETE /v3/credentials/c-1"
	f := newKeystoneFake(t, map[string]func() (int, string){
		mfaListKey:                replySequence(keystoneReply(200, totpList("c-1", "c-other")), keystoneReply(200, totpList())),
		del:                       keystoneReply(204, ""),
		patch:                     keystoneReply(503, `{"error": {"message": "unavailable"}}`),
		"GET /v3/credentials/c-1": keystoneReply(404, `{"error": {"message": "not found"}}`),
	})
	r := &userMFAResource{config: fakeConfig(f.srv.URL)}
	state := mfaState(t, r, "c-1")
	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
	errs := resp.Diagnostics.Errors()
	if len(errs) != 1 || errs[0].Summary() != "identity: turning off MFA" ||
		!strings.Contains(errs[0].Detail(), "-refresh=false") || !strings.Contains(errs[0].Detail(), "c-1") ||
		strings.Contains(errs[0].Detail(), "destroy again to finish") {
		t.Fatalf("delete diagnostics = %v; want one error naming c-1 that says to run again with -refresh=false", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("the resource was removed from state")
	}
	if want := []string{mfaListKey, del, mfaListKey, patch}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", f.paths(), want)
	}

	// A refresh of the state Delete left behind finds the credential gone and
	// removes the resource.
	readResp := resource.ReadResponse{State: resp.State}
	r.Read(ctx, resource.ReadRequest{State: resp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Fatal("a refresh after the failed destroy kept the resource; want it removed from state")
	}
}

// A create-before-destroy replacement creates the new object (a new credential,
// rules set) before it destroys the deposed one. That destroy must leave the
// rules the new object just set: it deletes only the old credential and warns,
// and sends no PATCH that would null the rules.
func TestUserMFAReplaceCreateBeforeDestroyKeepsMFA(t *testing.T) {
	ctx := context.Background()
	// The fake's handlers run on the server's goroutines.
	var mu sync.Mutex
	creds := []string{"c-old"}
	newCred := `{"credential": {"id": "c-1", "type": "totp", "user_id": "u-1", "blob": "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", "links": {}}}`
	f := newKeystoneFake(t, map[string]func() (int, string){
		"GET /v3/auth/tokens": keystoneReply(200, tokenBody),
		"POST /v3/credentials": func() (int, string) {
			mu.Lock()
			defer mu.Unlock()
			creds = append(creds, "c-1")
			return 201, newCred
		},
		"PATCH /v3/users/u-1": keystoneReply(200, mfaUserBody(`{"multi_factor_auth_rules": [["password", "totp"]]}`)),
		mfaListKey: func() (int, string) {
			mu.Lock()
			defer mu.Unlock()
			return 200, totpList(creds...)
		},
		"DELETE /v3/credentials/c-old": func() (int, string) {
			mu.Lock()
			defer mu.Unlock()
			creds = slices.DeleteFunc(creds, func(id string) bool { return id == "c-old" })
			return 204, ""
		},
	})
	r := &userMFAResource{config: fakeConfig(f.srv.URL)}
	deposed := mfaState(t, r, "c-old")

	plan, empty := mfaSchemaState(t, r)
	if d := plan.Set(ctx, &userMFAModel{
		ID: types.StringUnknown(), UserID: types.StringValue("u-1"), TOTPSecret: types.StringUnknown(),
		CredentialID: types.StringUnknown(), Enforced: types.BoolValue(true), Region: types.StringUnknown(),
	}); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	createResp := resource.CreateResponse{State: empty}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}

	deleteResp := resource.DeleteResponse{State: deposed}
	r.Delete(ctx, resource.DeleteRequest{State: deposed}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of the deposed object: %v", deleteResp.Diagnostics)
	}
	if w := deleteResp.Diagnostics.Warnings(); len(w) != 1 || w[0].Summary() != "MFA rules left in place" {
		t.Fatalf("delete warnings = %v; want MFA rules left in place", w)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var patches, deletes int
	for _, c := range f.calls {
		switch c.method {
		case "PATCH":
			patches++
			if strings.Contains(c.body, "null") {
				t.Fatalf("a PATCH nulls the MFA rules: %s", c.body)
			}
		case "DELETE":
			deletes++
			if c.path != "DELETE /v3/credentials/c-old" {
				t.Fatalf("deleted %s; want only the deposed credential c-old", c.path)
			}
		}
	}
	if patches != 1 || deletes != 1 {
		t.Fatalf("%d PATCH and %d DELETE requests; want the create's PATCH and one DELETE of c-old", patches, deletes)
	}
}

// Update checks the provider's token before restoring the rules. When that read
// fails it cannot tell whether the user is the provider's own, so it returns
// the error and sends nothing.
func TestUserMFAUpdateFailsClosedWhenTheTokenReadFails(t *testing.T) {
	ctx := context.Background()
	f := newKeystoneFake(t, map[string]func() (int, string){
		"GET /v3/auth/tokens": keystoneReply(500, `{"error": {"message": "boom"}}`),
	})
	r := &userMFAResource{config: fakeConfig(f.srv.URL)}
	plan, state := mfaSchemaState(t, r)
	drifted := userMFAModel{
		ID: types.StringValue("u-1"), UserID: types.StringValue("u-1"),
		TOTPSecret: types.StringValue("JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"), CredentialID: types.StringValue("c-1"),
		Enforced: types.BoolValue(false), Region: types.StringValue("region-one"),
	}
	if d := state.Set(ctx, &drifted); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	planned := drifted
	planned.Enforced = types.BoolValue(true)
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "identity: checking which user the provider authenticates as" {
		t.Fatalf("update diagnostics = %v; want the token read error", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(state.Raw) {
		t.Fatal("update changed the state although the token read failed")
	}
	if want := []string{"GET /v3/auth/tokens"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want only the token read", f.paths())
	}
}

// When the rules PATCH fails without a refusal and a read shows the rules in
// effect, Create keeps the credential and records it, tainted, along with the
// error, so the next apply or a destroy removes the rules and the credential
// instead of leaving the user with rules that need a passcode and no secret.
func TestUserMFACreateRecordsTheCredentialWhenTheAnswerIsLost(t *testing.T) {
	ctx := context.Background()
	f := newKeystoneFake(t, map[string]func() (int, string){
		"GET /v3/auth/tokens":  keystoneReply(200, tokenBody),
		"POST /v3/credentials": keystoneReply(201, credentialBody),
		"PATCH /v3/users/u-1":  keystoneReply(502, `{"error": {"message": "bad gateway"}}`),
		"GET /v3/users/u-1":    keystoneReply(200, mfaUserBody(`{"multi_factor_auth_rules": [["password", "totp"]]}`)),
	})
	r := &userMFAResource{config: fakeConfig(f.srv.URL)}
	plan, empty := mfaSchemaState(t, r)
	if d := plan.Set(ctx, &userMFAModel{
		ID: types.StringUnknown(), UserID: types.StringValue("u-1"), TOTPSecret: types.StringUnknown(),
		CredentialID: types.StringUnknown(), Enforced: types.BoolValue(true), Region: types.StringUnknown(),
	}); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.CreateResponse{State: empty}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	errs := resp.Diagnostics.Errors()
	if len(errs) != 1 || errs[0].Summary() != "identity: turning on MFA" || !strings.Contains(errs[0].Detail(), "was kept") {
		t.Fatalf("create diagnostics = %v; want one error saying the credential was kept", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create recorded nothing, so the kept credential and the rules are left behind")
	}
	var got userMFAModel
	resp.State.Get(ctx, &got)
	enrolled := f.bodyOf("POST /v3/credentials")["credential"].(map[string]any)["blob"]
	if got.ID.ValueString() != "u-1" || got.CredentialID.ValueString() != "c-1" || got.Enforced.ValueBool() ||
		got.TOTPSecret.ValueString() != enrolled {
		t.Fatalf("recorded id %s, credential %s, enforced %v, enrolled secret recorded %v; want u-1, c-1, false, true",
			got.ID, got.CredentialID, got.Enforced, got.TOTPSecret.ValueString() == enrolled)
	}
	if want := []string{"GET /v3/auth/tokens", "POST /v3/credentials", "PATCH /v3/users/u-1", "GET /v3/users/u-1"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", f.paths(), want)
	}
}

// Turning MFA on for the provider's own user would lock the provider out, so
// Create refuses before enrolling anything.
func TestUserMFACreateRefusesTheProvidersOwnUser(t *testing.T) {
	ctx := context.Background()
	f := newKeystoneFake(t, map[string]func() (int, string){"GET /v3/auth/tokens": keystoneReply(200, tokenBody)})
	r := &userMFAResource{config: fakeConfig(f.srv.URL)}
	plan, empty := mfaSchemaState(t, r)
	plan.Set(ctx, &userMFAModel{
		ID: types.StringUnknown(), UserID: types.StringValue("u-provider"), TOTPSecret: types.StringUnknown(),
		CredentialID: types.StringUnknown(), Enforced: types.BoolValue(true), Region: types.StringUnknown(),
	})
	resp := resource.CreateResponse{State: empty}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "Refusing to turn on MFA for the provider's own user" {
		t.Fatalf("create diagnostics = %v; want the refusal", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("create recorded state although it refused")
	}
	if want := []string{"GET /v3/auth/tokens"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want only the token read", f.paths())
	}
}

// An import of the provider's own user, whose leftover TOTP credential and
// password-only rules refresh as enforced = false, plans an update that would
// turn MFA on for that user and lock the provider out. Update refuses it, as
// Create does, before sending anything.
func TestUserMFAUpdateRefusesTheProvidersOwnUser(t *testing.T) {
	ctx := context.Background()
	f := newKeystoneFake(t, map[string]func() (int, string){"GET /v3/auth/tokens": keystoneReply(200, tokenBody)})
	r := &userMFAResource{config: fakeConfig(f.srv.URL)}
	plan, state := mfaSchemaState(t, r)
	imported := userMFAModel{
		ID: types.StringValue("u-provider"), UserID: types.StringValue("u-provider"),
		TOTPSecret: types.StringValue("JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"), CredentialID: types.StringValue("c-1"),
		Enforced: types.BoolValue(false), Region: types.StringValue("region-one"),
	}
	if d := state.Set(ctx, &imported); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	planned := imported
	planned.Enforced = types.BoolValue(true)
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "Refusing to turn on MFA for the provider's own user" {
		t.Fatalf("update diagnostics = %v; want the refusal", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(state.Raw) {
		t.Fatal("update changed the state although it refused")
	}
	if want := []string{"GET /v3/auth/tokens"}; fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want only the token read", f.paths())
	}
}

// Import takes the user ID and finds the user's TOTP credential; a user with
// none cannot be imported, and one with several gets a warning.
func TestUserMFAImport(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		list        string
		wantRemoved bool
		wantWarning bool
	}{
		{name: "one credential", list: `{"credentials": [` + strings.TrimSuffix(strings.TrimPrefix(credentialBody, `{"credential": `), "}") + `], "links": {"next": null}}`},
		{name: "two credentials", wantWarning: true, list: `{"credentials": [` +
			strings.TrimSuffix(strings.TrimPrefix(credentialBody, `{"credential": `), "}") + `, {"id": "c-2", "type": "totp", "user_id": "u-1", "blob": "MFRGGZDFMZTWQ2LK"}], "links": {"next": null}}`},
		{name: "no credential", wantRemoved: true, list: `{"credentials": [], "links": {"next": null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeystoneFake(t, map[string]func() (int, string){
				"GET /v3/credentials?type=totp&user_id=u-1": keystoneReply(200, tc.list),
				"GET /v3/users/u-1":                         keystoneReply(200, mfaUserBody(`{"multi_factor_auth_rules": [["password", "totp"]]}`)),
			})
			r := &userMFAResource{config: fakeConfig(f.srv.URL)}
			_, empty := mfaSchemaState(t, r)
			importResp := resource.ImportStateResponse{State: empty}
			r.ImportState(ctx, resource.ImportStateRequest{ID: "u-1"}, &importResp)
			readResp := resource.ReadResponse{State: importResp.State}
			r.Read(ctx, resource.ReadRequest{State: importResp.State}, &readResp)
			if readResp.Diagnostics.HasError() {
				t.Fatalf("read after import: %v", readResp.Diagnostics)
			}
			if readResp.State.Raw.IsNull() != tc.wantRemoved {
				t.Fatalf("removed = %v, want %v", readResp.State.Raw.IsNull(), tc.wantRemoved)
			}
			if (len(readResp.Diagnostics.Warnings()) > 0) != tc.wantWarning {
				t.Fatalf("warnings = %v, want a warning: %v", readResp.Diagnostics.Warnings(), tc.wantWarning)
			}
			if tc.wantRemoved {
				return
			}
			var got userMFAModel
			readResp.State.Get(ctx, &got)
			if got.UserID.ValueString() != "u-1" || got.CredentialID.ValueString() != "c-1" || !got.Enforced.ValueBool() {
				t.Fatalf("imported state = %+v; want user u-1, credential c-1, enforced", got)
			}
		})
	}
}

// An empty import ID names no user, so import refuses it before it sets any
// attribute, and no read follows.
func TestUserMFAImportRefusesAnEmptyID(t *testing.T) {
	ctx := context.Background()
	r := &userMFAResource{}
	_, empty := mfaSchemaState(t, r)
	resp := resource.ImportStateResponse{State: empty}
	r.ImportState(ctx, resource.ImportStateRequest{ID: ""}, &resp)
	var summaries []string
	for _, d := range resp.Diagnostics.Errors() {
		summaries = append(summaries, d.Summary())
	}
	if len(summaries) != 1 || summaries[0] != "Import needs a user ID" || !resp.State.Raw.IsNull() {
		t.Fatalf("errors = %v, state null %v; want [Import needs a user ID] and no state", summaries, resp.State.Raw.IsNull())
	}
}

// A 200 without the credential or the user object decodes to nil. Read reports
// it as an error instead of crashing on it, and keeps the resource in state.
func TestUserMFAReadRefusesAnAnswerWithoutTheObject(t *testing.T) {
	ctx := context.Background()
	for name, routes := range map[string]map[string]func() (int, string){
		"credential": {"GET /v3/credentials/c-1": keystoneReply(200, `{}`)},
		"user": {
			"GET /v3/credentials/c-1": keystoneReply(200, credentialBody),
			"GET /v3/users/u-1":       keystoneReply(200, `{}`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newKeystoneFake(t, routes)
			r := &userMFAResource{config: fakeConfig(f.srv.URL)}
			_, state := mfaSchemaState(t, r)
			if d := state.Set(ctx, &userMFAModel{
				ID: types.StringValue("u-1"), UserID: types.StringValue("u-1"),
				TOTPSecret: types.StringValue("JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"), CredentialID: types.StringValue("c-1"),
				Enforced: types.BoolValue(true), Region: types.StringValue("region-one"),
			}); d.HasError() {
				t.Fatalf("building the state: %v", d)
			}
			resp := resource.ReadResponse{State: state}
			r.Read(ctx, resource.ReadRequest{State: state}, &resp)
			if !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
				t.Fatalf("read: diags %v, state null %v; want an error and the resource kept", resp.Diagnostics, resp.State.Raw.IsNull())
			}
		})
	}
}

// An empty user ID would drop the user_id filter, and Keystone would then list
// every user's TOTP credentials, so the lookup refuses it before any request.
func TestFindTOTPCredentialRefusesEmptyUserID(t *testing.T) {
	f := newKeystoneFake(t, map[string]func() (int, string){})
	cred, count, err := findTOTPCredential(context.Background(), f.bareClient(), "")
	if err == nil || cred != nil || count != 0 {
		t.Fatalf("findTOTPCredential(\"\") = %v, %d, %v; want nil, 0 and an error", cred, count, err)
	}
	if got := f.paths(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// An empty user_id names no user, so the schema refuses it at plan time.
func TestUserMFASchemaRefusesEmptyUserID(t *testing.T) {
	ctx := context.Background()
	var sch resource.SchemaResponse
	(&userMFAResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	attr, ok := sch.Schema.Attributes["user_id"].(schema.StringAttribute)
	if !ok || len(attr.Validators) == 0 {
		t.Fatal("user_id has no string validators")
	}
	var resp validator.StringResponse
	for _, v := range attr.Validators {
		v.ValidateString(ctx, validator.StringRequest{Path: path.Root("user_id"), ConfigValue: types.StringValue("")}, &resp)
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("an empty user_id passes the schema's validators")
	}
}

// Terraform prints a provider's diagnostics as they are, so the error for a
// rejected totp_secret states the rule but never the value, nor any part of it.
// The validators come from the schema, as Terraform runs them.
func TestUserMFASecretValidatorNeverPrintsTheSecret(t *testing.T) {
	ctx := context.Background()
	var sch resource.SchemaResponse
	(&userMFAResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	attr, ok := sch.Schema.Attributes["totp_secret"].(schema.StringAttribute)
	if !ok || len(attr.Validators) == 0 {
		t.Fatal("totp_secret has no string validators")
	}
	validate := func(v types.String) validator.StringResponse {
		var resp validator.StringResponse
		for _, val := range attr.Validators {
			val.ValidateString(ctx, validator.StringRequest{Path: path.Root("totp_secret"), ConfigValue: v}, &resp)
		}
		return resp
	}
	for _, bad := range []string{
		"JBSWY3DPEHPK3PXP",                 // 16 characters
		"jbswy3dpehpk3pxpjbswy3dpehpk3pxp", // lower case, 32 characters
		"JBSWY3DPEHPK3PXPJBSWY3DPEHP",      // 27 characters do not decode
		"JBSWY3DPEHPK3PXPJBSWY3DPE1",       // 1 is not base32
	} {
		resp := validate(types.StringValue(bad))
		if resp.Diagnostics.ErrorsCount() != 1 || len(resp.Diagnostics) != 1 {
			t.Fatalf("%d-character secret: %d diagnostics; want exactly one error", len(bad), len(resp.Diagnostics))
		}
		d := resp.Diagnostics[0]
		text := d.Summary() + "\n" + d.Detail()
		for i := 0; i+8 <= len(bad); i++ {
			if strings.Contains(text, bad[i:i+8]) {
				t.Fatalf("the error for a rejected %d-character secret shows part of it (%d characters from %d): %s",
					len(bad), 8, i, d.Summary())
			}
		}
		if !strings.Contains(d.Detail(), totpSecretRule) {
			t.Fatalf("the error for a rejected %d-character secret does not state the rule", len(bad))
		}
	}
	for _, good := range []types.String{
		types.StringValue("JBSWY3DPEHPK3PXPJBSWY3DPEH"),       // 26 characters, as the PCD UI generates
		types.StringValue("JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"), // 32 characters
		types.StringValue("JBSWY3DPEHPK3PXPJBSWY3DPEH======"), // 26 characters, padded
		types.StringNull(),
		types.StringUnknown(),
	} {
		if resp := validate(good); len(resp.Diagnostics) != 0 {
			t.Errorf("totp_secret %s: %d diagnostics; want none", good, len(resp.Diagnostics))
		}
	}
}

// totp_secret is sensitive, so plans, applies and logs never print it; and
// changing it or user_id replaces the resource: Update only restores the rules,
// so an in-place change would record a secret or a user Keystone does not hold.
func TestUserMFASchemaKeepsTheSecretSensitiveAndReplaces(t *testing.T) {
	ctx := context.Background()
	var sch resource.SchemaResponse
	(&userMFAResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	if !s.Attributes["totp_secret"].IsSensitive() {
		t.Error("totp_secret is not sensitive")
	}
	// objectWith is an object of the schema whose attributes are all null but name.
	objectWith := func(name, value string) tftypes.Value {
		typ := s.Type().TerraformType(ctx).(tftypes.Object)
		vals := map[string]tftypes.Value{}
		for n, at := range typ.AttributeTypes {
			vals[n] = tftypes.NewValue(at, nil)
		}
		vals[name] = tftypes.NewValue(tftypes.String, value)
		return tftypes.NewValue(typ, vals)
	}
	for name, values := range map[string][2]string{
		"user_id":     {"u-1", "u-2"},
		"totp_secret": {"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", "MFRGGZDFMZTWQ2LKMFRGGZDFMZTWQ2LK"},
	} {
		attr, ok := s.Attributes[name].(schema.StringAttribute)
		if !ok {
			t.Fatalf("%s is not a string attribute", name)
		}
		state := tfsdk.State{Schema: s, Raw: objectWith(name, values[0])}
		plan := tfsdk.Plan{Schema: s, Raw: objectWith(name, values[1])}
		req := planmodifier.StringRequest{
			Path: path.Root(name), Config: tfsdk.Config{Schema: s, Raw: plan.Raw},
			ConfigValue: types.StringValue(values[1]), Plan: plan, PlanValue: types.StringValue(values[1]),
			State: state, StateValue: types.StringValue(values[0]),
		}
		replace := false
		for _, m := range attr.PlanModifiers {
			resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
			m.PlanModifyString(ctx, req, resp)
			replace = replace || resp.RequiresReplace
		}
		if !replace {
			t.Errorf("changing %s plans an in-place update; want a replacement", name)
		}
	}
}
