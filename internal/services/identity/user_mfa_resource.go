// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// terraform-provider-openstack v3.4.0 has no TOTP enrollment resource. This one
// does what the PCD UI's self-service "Multifactor Authentication" settings do
// (a Keystone credential of type totp plus the user option
// multi_factor_auth_rules), for any user an administrator manages.

package identity

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/credentials"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

var (
	_ resource.Resource                = (*userMFAResource)(nil)
	_ resource.ResourceWithConfigure   = (*userMFAResource)(nil)
	_ resource.ResourceWithImportState = (*userMFAResource)(nil)
)

// NewUserMFAResource is the factory registered with the provider.
func NewUserMFAResource() resource.Resource {
	return &userMFAResource{}
}

type userMFAResource struct {
	config *clients.Config
}

type userMFAModel struct {
	ID           types.String `tfsdk:"id"`
	UserID       types.String `tfsdk:"user_id"`
	TOTPSecret   types.String `tfsdk:"totp_secret"`
	CredentialID types.String `tfsdk:"credential_id"`
	Enforced     types.Bool   `tfsdk:"enforced"`
	Region       types.String `tfsdk:"region"`
}

// mfaRules is the rule set the PCD UI writes when a user turns MFA on: every
// login needs a password and a TOTP passcode.
var mfaRules = [][]string{{"password", "totp"}}

// totpSecretPattern accepts exactly the secrets Keystone can use. Keystone pads
// the secret with "=" to a multiple of 8 characters and base32-decodes it, and
// its TOTP library needs a key of at least 128 bits (16 bytes, 26 characters).
// So: upper-case base32 of at least 26 characters whose last block, once
// padded, is valid base32 (2, 4, 5 or 7 characters plus at most the padding
// that completes the block, or a full block). Keystone logs a secret it cannot
// use and rejects every passcode, which with the [["password", "totp"]] rules
// locks the user out, so such a secret is refused at plan time. The PCD UI's
// own secrets are 26 characters.
var totpSecretPattern = regexp.MustCompile(`^[A-Z2-7]{24}(?:[A-Z2-7]{8})*(?:[A-Z2-7]{2}={0,6}|[A-Z2-7]{4}={0,4}|[A-Z2-7]{5}={0,3}|[A-Z2-7]{7}=?|[A-Z2-7]{8})$`)

// totpSecretRule is the rule totp_secret must meet, as diagnostics state it.
const totpSecretRule = `must be upper-case base32 (A-Z, 2-7) that decodes to whole bytes, at least 26 characters (128 bits), optionally padded with "="`

// mfaRecoveryAdvice says how to recover a user whose remaining TOTP
// credentials nobody holds the secret of. The description and the warning of a
// destroy that leaves the MFA rules in place both give it.
const mfaRecoveryAdvice = "If nobody holds the secret of any remaining credential (for example the only one left " +
	"was left behind by an interrupted create), the user cannot log in until the MFA rules are removed or a new " +
	"secret is enrolled. To remove the rules when one TOTP credential remains, import it into this resource and " +
	"destroy it, which removes the rules and the credential. To enroll a new secret, create this resource for the " +
	"user again. Deleting a credential alone leaves the rules in place."

// totpSecretValidator checks totp_secret against totpSecretPattern. It exists
// because the stock stringvalidator.RegexMatches quotes the rejected value in
// its error, and Terraform prints a provider's diagnostics as they are, with no
// redaction of sensitive values: a rejected secret would reach the console and
// CI logs. This one states the rule and never the value.
type totpSecretValidator struct{}

func (totpSecretValidator) Description(context.Context) string {
	return "value " + totpSecretRule
}

func (v totpSecretValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (totpSecretValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if !totpSecretPattern.MatchString(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid TOTP secret",
			"Attribute totp_secret "+totpSecretRule+". The value is not shown because it is sensitive.")
	}
}

func (r *userMFAResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_user_mfa"
}

func (r *userMFAResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Turns on multi-factor authentication (MFA) for a user, as the PCD UI's " +
			"\"Multifactor Authentication\" user setting does: it enrolls a TOTP secret as a Keystone credential " +
			"and sets the user's `multi_factor_auth_rules` to `[[\"password\", \"totp\"]]`, so every login needs " +
			"the password and a six-digit passcode from an authenticator app. Destroying the resource deletes " +
			"its TOTP credential and, when the user has no other TOTP credential, removes the user's MFA " +
			"rules, which lets the user log in with any method Keystone allows again.\n\n" +
			"Once MFA is on, Keystone checks the rules for every way the user logs in: the user can no longer " +
			"authenticate with an application credential (for example one from " +
			"`pcd_identity_application_credential`) or through single sign-on. Do not turn MFA on for a user " +
			"that any provider configuration authenticates as, because the provider cannot supply a passcode. " +
			"The provider refuses it only for the user of the provider configuration this resource uses.\n\n" +
			"The TOTP secret is stored in Terraform state (marked sensitive). Give it to the user's authenticator " +
			"app as an `otpauth://totp/<issuer>:<user name>?secret=<totp_secret>&issuer=<issuer>` URI, the form " +
			"the PCD UI shows as a QR code. Keystone accepts a passcode from any TOTP credential the user has, so " +
			"a secret the user also enrolled in the PCD UI keeps working. While the user has another TOTP credential " +
			"(the new one during a create-before-destroy replacement, or a secret the user enrolled themselves), " +
			"destroying the resource deletes only its own credential, keeps the MFA rules and warns that the MFA " +
			"rules were left in place. " + mfaRecoveryAdvice + " To manage MFA a " +
			"user already set up in the PCD UI, import it instead of creating it. Create replaces any MFA rules the " +
			"user already had with `[[\"password\", \"totp\"]]`, and a destroy that turns MFA off clears the " +
			"rules rather than restoring the previous ones. Rename or move this resource with a `moved` block: " +
			"destroying one instance while another is created for the same user in the same apply can leave MFA " +
			"off until the next apply.\n\n" +
			"If the TOTP credential is deleted outside Terraform, the next refresh drops the resource and the " +
			"next apply turns MFA on again; unless `totp_secret` is set, that enrolls a new secret, which the user " +
			"must add to their authenticator app. Until that apply the MFA rules may still be in place, and a user " +
			"with no other TOTP credential then cannot log in. To turn MFA off for such a user, apply first and " +
			"then destroy: a destroy alone finds nothing to destroy and leaves the rules in place.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The user ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"user_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The user to turn MFA on for. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"totp_secret": schema.StringAttribute{
				Optional:  true,
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The TOTP secret, base32 in upper case (`A`-`Z`, `2`-`7`) that decodes to " +
					"whole bytes, at least 26 characters (128 bits, the least Keystone accepts). Omit it and the " +
					"provider generates a 32-character secret (160 random bits). " +
					"Either way it is stored in state. Changing it forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
				Validators:    []validator.String{totpSecretValidator{}},
			},
			"credential_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The ID of the Keystone credential that holds the TOTP secret.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"enforced": schema.BoolAttribute{
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether Keystone requires a TOTP passcode for every login of the user. " +
					"Always `true` after an apply; a refresh reads `false` when the user's MFA rules were changed " +
					"outside Terraform, and the next apply restores them.",
			},
			"region": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The region whose identity endpoint the provider uses; Keystone is global. Defaults to the provider's region. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *userMFAResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *userMFAResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userMFAModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	userID := plan.UserID.ValueString()
	scope, err := currentTokenScope(ctx, client)
	if err != nil {
		resp.Diagnostics.AddError("identity: checking which user the provider authenticates as", err.Error())
		return
	}
	if scope.UserID == userID {
		resp.Diagnostics.AddError("Refusing to turn on MFA for the provider's own user",
			fmt.Sprintf("User %s is the user this provider authenticates as. With MFA on, Keystone would refuse "+
				"the provider's next login, which carries no TOTP passcode. Run Terraform as another administrator "+
				"to manage MFA for this user.", userID))
		return
	}

	secret := plan.TOTPSecret.ValueString()
	if plan.TOTPSecret.IsNull() || plan.TOTPSecret.IsUnknown() {
		secret, err = generateTOTPSecret()
		if err != nil {
			resp.Diagnostics.AddError("identity: generating a TOTP secret", err.Error())
			return
		}
	}

	plan.ID = types.StringValue(userID)
	plan.TOTPSecret = types.StringValue(secret)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}

	credentialID, err := enableMFA(ctx, client, userID, secret)
	if err != nil {
		if credentialID != "" {
			// The credential exists, kept because the rules may be in effect or
			// because deleting it failed: record it, tainted, so the next apply
			// or a destroy removes the rules and the credential.
			plan.CredentialID = types.StringValue(credentialID)
			plan.Enforced = types.BoolValue(false)
			if !tfstate.RecordCreated(ctx, resp, &plan) {
				return
			}
		}
		resp.Diagnostics.AddError("identity: turning on MFA", err.Error())
		return
	}

	plan.CredentialID = types.StringValue(credentialID)
	plan.Enforced = types.BoolValue(true)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userMFAResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userMFAModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	userID := state.UserID.ValueString()
	var cred *credentials.Credential
	if id := state.CredentialID.ValueString(); id != "" {
		cred, err = credentials.Get(ctx, client, id).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				resp.Diagnostics.AddWarning("TOTP credential not found",
					fmt.Sprintf("TOTP credential %s of user %s no longer exists, so the resource was removed from "+
						"state. If the user was deleted, Keystone deleted the credential with it. Otherwise the "+
						"credential was deleted outside Terraform. Deleting a credential alone leaves the user's MFA "+
						"rules in place, so a user with no other TOTP credential may be unable to log in until the next "+
						"apply, which turns MFA on again with a new credential. Unless totp_secret is set in the "+
						"configuration, that apply enrolls a new secret, which the user must add to their authenticator "+
						"app. To turn MFA off instead, let that apply run and then destroy the resource; removing it "+
						"from the configuration now may leave the rules in place.", id, userID))
				resp.State.RemoveResource(ctx)
				return
			}
			resp.Diagnostics.AddError("identity: reading TOTP credential", err.Error())
			return
		}
		// A 200 without the object decodes to nil. It is not a not-found: that
		// would drop MFA that is still on from state.
		if cred == nil {
			resp.Diagnostics.AddError("identity: reading TOTP credential",
				fmt.Sprintf("The Identity API answered without a credential object for ID %q.", id))
			return
		}
	} else {
		// Import: the ID is the user's, so find the user's TOTP credential.
		var count int
		cred, count, err = findTOTPCredential(ctx, client, userID)
		if err != nil {
			resp.Diagnostics.AddError("identity: listing TOTP credentials", err.Error())
			return
		}
		if cred == nil {
			resp.State.RemoveResource(ctx)
			return
		}
		if count > 1 {
			resp.Diagnostics.AddWarning("Several TOTP credentials",
				fmt.Sprintf("User %s has %d TOTP credentials, and Keystone accepts a passcode from any of them. "+
					"Imported %s; destroying this resource deletes only that one.", userID, count, cred.ID))
		}
	}

	user, err := users.Get(ctx, client, userID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading user", err.Error())
		return
	}
	if user == nil {
		resp.Diagnostics.AddError("identity: reading user",
			fmt.Sprintf("The Identity API answered without a user object for ID %q.", userID))
		return
	}

	state.ID = types.StringValue(userID)
	state.CredentialID = types.StringValue(cred.ID)
	state.TOTPSecret = types.StringValue(cred.Blob)
	state.Enforced = types.BoolValue(mfaEnforced(user.Options))
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update runs only when a refresh found the user's MFA rules changed outside
// Terraform (enforced is false; its planned value is always true). Every other
// attribute forces replacement. Like Create, it refuses the provider's own
// user, which an import of that user's leftover TOTP credential can bring here.
func (r *userMFAResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state userMFAModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !state.Enforced.ValueBool() {
		client, err := r.config.ForRegion(plan.Region.ValueString()).IdentityV3Client()
		if err != nil {
			resp.Diagnostics.AddError("identity: building v3 client", err.Error())
			return
		}
		userID := state.UserID.ValueString()
		scope, err := currentTokenScope(ctx, client)
		if err != nil {
			resp.Diagnostics.AddError("identity: checking which user the provider authenticates as", err.Error())
			return
		}
		if scope.UserID == userID {
			resp.Diagnostics.AddError("Refusing to turn on MFA for the provider's own user",
				fmt.Sprintf("User %s is the user this provider authenticates as. With MFA on, Keystone would refuse "+
					"the provider's next login, which carries no TOTP passcode. Run Terraform as another administrator "+
					"to manage MFA for this user, or stop managing it here with `terraform state rm` on this resource.", userID))
			return
		}
		if err := setMFARules(ctx, client, userID); err != nil {
			resp.Diagnostics.AddError("identity: restoring the user's MFA rules", err.Error())
			return
		}
	}
	plan.Enforced = types.BoolValue(true)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userMFAResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userMFAModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	// The rules stay while the user has another TOTP credential: in a
	// create-before-destroy replacement that is the new object's credential,
	// whose rules this destroy of the deposed object must not clear, and
	// otherwise a secret the user enrolled. A failed listing changes nothing.
	userID, credentialID := state.UserID.ValueString(), state.CredentialID.ValueString()
	all, err := listTOTPCredentials(ctx, client, userID)
	if err != nil {
		resp.Diagnostics.AddError("identity: listing TOTP credentials", err.Error())
		return
	}
	if len(otherCredentialIDs(all, credentialID)) == 0 {
		if err := disableMFA(ctx, client, userID, credentialID); err != nil {
			resp.Diagnostics.AddError("identity: turning off MFA", err.Error())
		}
		return
	}
	if err := deleteTOTPCredential(ctx, client, credentialID); err != nil {
		resp.Diagnostics.AddError("identity: deleting the TOTP credential", err.Error())
		return
	}
	// Terraform destroys a current and a deposed object of the same user
	// concurrently, so the other credential may have been deleted since the
	// first listing. Listing again lets the destroy that deletes last see that
	// none is left and clear the rules, which would otherwise require a
	// passcode no credential can give. A retried destroy (with -refresh=false,
	// since a refresh drops the resource once its credential is gone) finds its
	// own credential gone (a 404 on its DELETE counts as done) and decides again
	// from a fresh listing.
	all, err = listTOTPCredentials(ctx, client, userID)
	if err != nil {
		resp.Diagnostics.AddError("identity: listing TOTP credentials",
			fmt.Sprintf("TOTP credential %s was deleted, but listing the user's remaining TOTP credentials failed, so "+
				"the user's MFA rules were left as they are. To finish, run the same Terraform command again with "+
				"-refresh=false: a refresh would remove this resource from state, because its credential is gone, and "+
				"leave the rules in place: %v", credentialID, err))
		return
	}
	remaining := otherCredentialIDs(all, credentialID)
	if len(remaining) == 0 {
		if err := clearMFARules(ctx, client, userID); err != nil {
			resp.Diagnostics.AddError("identity: turning off MFA",
				fmt.Sprintf("TOTP credential %s was deleted and the user has no other TOTP credential, but removing the "+
					"user's MFA rules failed, so the user cannot log in until they are removed. To finish, run the same "+
					"Terraform command again with -refresh=false: a refresh would remove this resource from state, because "+
					"its credential is gone, and leave the rules in place: %v", credentialID, err))
		}
		return
	}
	resp.Diagnostics.AddWarning("MFA rules left in place",
		fmt.Sprintf("User %s still has %d TOTP credential(s) (%s), so destroy deleted only TOTP credential %s and "+
			"left the user's MFA rules as they are. Another credential remains when the resource is replaced "+
			"create-before-destroy (the new credential) or when the user enrolled a secret of their own. "+
			mfaRecoveryAdvice, userID, len(remaining), strings.Join(remaining, ", "), credentialID))
}

// otherCredentialIDs returns the IDs of the credentials in all other than
// own, sorted.
func otherCredentialIDs(all []credentials.Credential, own string) []string {
	var ids []string
	for _, c := range all {
		if c.ID != own {
			ids = append(ids, c.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// ImportState takes the user ID; Read finds the user's TOTP credential. An
// empty ID names no user, so it is refused before anything is read.
func (r *userMFAResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID == "" {
		resp.Diagnostics.AddError("Import needs a user ID",
			"Pass the ID of the user whose MFA to import, which `pcdctl user list` shows.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), req.ID)...)
}

// generateTOTPSecret returns 160 random bits as unpadded base32, the secret
// size RFC 4226 recommends for HMAC-SHA1.
func generateTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// enableMFA enrolls secret as the user's TOTP credential, then requires a
// password and a passcode for every login. The credential comes first so the
// rule never applies to a user with no passcode to give. When the rule cannot
// be set, the credential is deleted again, unless a read shows the rule is in
// effect after all (or cannot tell): deleting it then would lock the user out.
// The credential ID is returned with the error whenever the credential is kept
// or its delete failed, so the caller can record it.
func enableMFA(ctx context.Context, client *gophercloud.ServiceClient, userID, secret string) (string, error) {
	cred, err := credentials.Create(ctx, client, credentials.CreateOpts{Blob: secret, Type: "totp", UserID: userID}).Extract()
	if err != nil {
		return "", fmt.Errorf("creating the TOTP credential for user %s: %w", userID, err)
	}
	// A 201 without the object decodes to nil: there is no credential ID to
	// record or roll back, and the rules are not set without one.
	if cred == nil {
		return "", fmt.Errorf("creating the TOTP credential for user %s: the Identity API answered without a credential object", userID)
	}
	if err := setMFARules(ctx, client, userID); err != nil {
		// A 4xx is Keystone refusing the PATCH, so the rules are as they were.
		// Any other failure, such as a 5xx, a gateway error or a lost answer,
		// does not prove Keystone left them alone, so the user is read back
		// first and the credential kept when the rules require a passcode.
		var status gophercloud.ErrUnexpectedResponseCode
		refused := errors.As(err, &status) && status.Actual >= http.StatusBadRequest && status.Actual < http.StatusInternalServerError
		if !refused {
			user, getErr := clients.RequireObject(users.Get(ctx, client, userID).Extract())
			if getErr != nil {
				return cred.ID, fmt.Errorf("requiring TOTP for user %s: %w; reading the user back to see whether the "+
					"MFA rules took effect also failed: %v. The TOTP credential %s created for it was kept, because the "+
					"rules may be in effect; the next apply reconciles it", userID, err, getErr, cred.ID)
			}
			if mfaEnforced(user.Options) {
				return cred.ID, fmt.Errorf("requiring TOTP for user %s: %w; the user's MFA rules are in effect "+
					"nonetheless, set by this request with its answer lost or already there before it. The TOTP "+
					"credential %s created for it was kept, and the next apply reconciles it", userID, err, cred.ID)
			}
		}
		delErr := credentials.Delete(ctx, client, cred.ID).ExtractErr()
		if delErr != nil && !gophercloud.ResponseCodeIs(delErr, http.StatusNotFound) {
			return cred.ID, fmt.Errorf("requiring TOTP for user %s: %w; deleting the TOTP credential %s created "+
				"for it also failed: %v", userID, err, cred.ID, delErr)
		}
		return "", fmt.Errorf("requiring TOTP for user %s: %w (the TOTP credential created for it was deleted again)", userID, err)
	}
	return cred.ID, nil
}

// setMFARules sets the user's MFA options and nothing else: the PATCH body
// holds only options, so the user's name, password and enabled flag are left
// alone (the PCD UI's own request also forces enabled = true).
func setMFARules(ctx context.Context, client *gophercloud.ServiceClient, userID string) error {
	_, err := users.Update(ctx, client, userID, users.UpdateOpts{Options: map[users.Option]any{
		users.MultiFactorAuthRules:   mfaRules,
		users.MultiFactorAuthEnabled: true,
	}}).Extract()
	return err
}

// disableMFA removes the user's MFA options, then the credential. Keystone
// deletes an option sent as null, which puts the user back to Keystone's
// default (any enabled login method), the state before MFA was turned on. The
// PCD UI writes [["password"]] instead, which would also lock the user out of
// application credentials and single sign-on. The rules go first so there is
// no moment in which the user must give a passcode but has no credential. A
// 404 at either step means it is already gone.
func disableMFA(ctx context.Context, client *gophercloud.ServiceClient, userID, credentialID string) error {
	if err := clearMFARules(ctx, client, userID); err != nil {
		return err
	}
	return deleteTOTPCredential(ctx, client, credentialID)
}

// clearMFARules removes the user's MFA options by sending them as null; a 404
// means the user, and with it the rules, is already gone. It is the one place
// that sends this PATCH.
func clearMFARules(ctx context.Context, client *gophercloud.ServiceClient, userID string) error {
	_, err := users.Update(ctx, client, userID, users.UpdateOpts{Options: map[users.Option]any{
		users.MultiFactorAuthRules:   nil,
		users.MultiFactorAuthEnabled: nil,
	}}).Extract()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return fmt.Errorf("removing the MFA rules of user %s: %w", userID, err)
	}
	return nil
}

// deleteTOTPCredential deletes one TOTP credential; a 404 means it is already
// gone, and an empty ID names none.
func deleteTOTPCredential(ctx context.Context, client *gophercloud.ServiceClient, credentialID string) error {
	if credentialID == "" {
		return nil
	}
	if err := credentials.Delete(ctx, client, credentialID).ExtractErr(); err != nil &&
		!gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return fmt.Errorf("deleting TOTP credential %s: %w", credentialID, err)
	}
	return nil
}

// listTOTPCredentials returns the user's TOTP credentials. It is the one place
// that lists them.
func listTOTPCredentials(ctx context.Context, client *gophercloud.ServiceClient, userID string) ([]credentials.Credential, error) {
	// An empty user ID drops the filter, and Keystone then lists every
	// user's TOTP credentials.
	if userID == "" {
		return nil, fmt.Errorf("no user ID to look up TOTP credentials for")
	}
	pages, err := credentials.List(client, credentials.ListOpts{UserID: userID, Type: "totp"}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return credentials.ExtractCredentials(pages)
}

// findTOTPCredential returns the first of the user's TOTP credentials and how
// many there are; nil when the user has none.
func findTOTPCredential(ctx context.Context, client *gophercloud.ServiceClient, userID string) (*credentials.Credential, int, error) {
	all, err := listTOTPCredentials(ctx, client, userID)
	if err != nil {
		return nil, 0, err
	}
	if len(all) == 0 {
		return nil, 0, nil
	}
	return &all[0], len(all), nil
}

// mfaEnforced reports whether the user's options make Keystone require a TOTP
// passcode for every login: multi_factor_auth_enabled is not false (Keystone
// treats a missing value as true) and every rule names totp. A rule without
// totp, such as the PCD UI's [["password"]], lets a login through without one.
func mfaEnforced(options map[string]any) bool {
	if enabled, ok := options[string(users.MultiFactorAuthEnabled)].(bool); ok && !enabled {
		return false
	}
	rules, ok := options[string(users.MultiFactorAuthRules)].([]any)
	if !ok || len(rules) == 0 {
		return false
	}
	for _, rule := range rules {
		methods, ok := rule.([]any)
		if !ok {
			return false
		}
		hasTOTP := false
		for _, m := range methods {
			if m == "totp" {
				hasTOTP = true
			}
		}
		if !hasTOTP {
			return false
		}
	}
	return true
}
