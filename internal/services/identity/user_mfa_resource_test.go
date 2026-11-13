// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/credentials"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

const (
	testAccMFAUser     = "tf-acc-mfa-user"
	testAccMFAPassword = "Tf-Acc-Passw0rd!"
	testAccMFASecret   = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
)

// TestAccIdentityUserMFA turns MFA on for a new user and proves Keystone then
// needs a passcode: a password-and-passcode login succeeds and a password-only
// login fails. It imports the resource, restores rules changed outside
// Terraform, and finally removes the resource and checks that a password-only
// login works again. Opt in with PCD_ACC_MFA=1.
func TestAccIdentityUserMFA(t *testing.T) {
	if os.Getenv("PCD_ACC_MFA") == "" {
		t.Skip("PCD_ACC_MFA not set; set it to 1 to create a user with MFA and log in as it")
	}
	const rn = "pcd_identity_user_mfa.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckUserMFADestroy(t),
			testAccCheckUserDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccUserMFAConfig(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "enforced", "true"),
					resource.TestCheckResourceAttr(rn, "totp_secret", testAccMFASecret),
					resource.TestCheckResourceAttrPair(rn, "id", "pcd_identity_user.test", "id"),
					testAccCheckTOTPCredential(t, rn),
					testAccCheckMFALogin(t, true),
					testAccCheckMFALogin(t, false),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
			{
				// The PCD UI's "disable" writes [["password"]]; the next plan
				// restores the rules in place.
				PreConfig: func() { testAccSetMFARules(t, [][]string{{"password"}}) },
				Config:    testAccUserMFAConfig(true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "enforced", "true"),
					testAccCheckMFALogin(t, false),
				),
			},
			{
				// Destroying the resource removes the rules and the credential.
				Config: testAccUserMFAConfig(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckNoMFARules(t, "pcd_identity_user.test"),
					testAccCheckPasswordOnlyLoginWorks(t),
				),
			},
		},
	})
}

// TestAccIdentityUserMFA_replaceCreateBeforeDestroy replaces the resource with
// create_before_destroy by changing its secret: Terraform creates the new
// credential first, then destroys the deposed object, which must leave MFA on
// because the new credential remains. Removing the resource afterward turns
// MFA off. Opt in with PCD_ACC_MFA=1.
func TestAccIdentityUserMFA_replaceCreateBeforeDestroy(t *testing.T) {
	if os.Getenv("PCD_ACC_MFA") == "" {
		t.Skip("PCD_ACC_MFA not set; set it to 1 to create a user with MFA and log in as it")
	}
	const (
		rn         = "pcd_identity_user_mfa.test"
		nextSecret = "KRUGS4ZANFZSAYJAONSWG4TFOQQGC3TE"
	)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckUserMFADestroy(t),
			testAccCheckUserDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccUserMFACreateBeforeDestroyConfig(testAccMFASecret),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "enforced", "true"),
					testAccCheckTOTPCredential(t, rn),
					testAccCheckMFALogin(t, false),
				),
			},
			{
				// A new secret replaces the resource, the new object first.
				Config: testAccUserMFACreateBeforeDestroyConfig(nextSecret),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionCreateBeforeDestroy)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "enforced", "true"),
					testAccCheckOnlyTOTPCredentialEnforced(t, rn),
					testAccCheckMFALogin(t, false),
				),
			},
			{
				// With no other credential left, destroying the resource removes
				// the rules and the credential.
				Config: testAccUserMFAConfig(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckNoMFARules(t, "pcd_identity_user.test"),
					testAccCheckPasswordOnlyLoginWorks(t),
				),
			},
		},
	})
}

func testAccUserMFACreateBeforeDestroyConfig(secret string) string {
	return fmt.Sprintf(`
resource "pcd_identity_user" "test" {
  name     = %q
  password = %q
}

resource "pcd_identity_user_mfa" "test" {
  user_id     = pcd_identity_user.test.id
  totp_secret = %q

  lifecycle {
    create_before_destroy = true
  }
}
`, testAccMFAUser, testAccMFAPassword, secret)
}

// testAccCheckOnlyTOTPCredentialEnforced checks that the user of n has MFA
// rules that all require totp and exactly one TOTP credential, the one n
// records.
func testAccCheckOnlyTOTPCredentialEnforced(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		userID := rs.Primary.Attributes["user_id"]
		// An empty ID would drop the user_id filter from the list below.
		if userID == "" {
			return fmt.Errorf("%s has no user_id in state", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		u, err := clients.RequireObject(users.Get(context.Background(), client, userID).Extract())
		if err != nil {
			return err
		}
		rules, _ := u.Options[string(users.MultiFactorAuthRules)].([]any)
		if len(rules) == 0 {
			return fmt.Errorf("user %s has no MFA rules after the replacement", userID)
		}
		for _, rule := range rules {
			if methods, _ := rule.([]any); !strings.Contains(fmt.Sprint(methods), "totp") {
				return fmt.Errorf("user %s has MFA rule %v without totp", userID, methods)
			}
		}
		if enabled, ok := u.Options[string(users.MultiFactorAuthEnabled)].(bool); ok && !enabled {
			return fmt.Errorf("user %s has multi_factor_auth_enabled = false", userID)
		}
		pages, err := credentials.List(client, credentials.ListOpts{UserID: userID, Type: "totp"}).AllPages(context.Background())
		if err != nil {
			return err
		}
		all, err := credentials.ExtractCredentials(pages)
		if err != nil {
			return err
		}
		if len(all) != 1 || all[0].ID != rs.Primary.Attributes["credential_id"] {
			ids := make([]string, 0, len(all))
			for _, c := range all {
				ids = append(ids, c.ID)
			}
			return fmt.Errorf("user %s has TOTP credentials %v; want only %s", userID, ids, rs.Primary.Attributes["credential_id"])
		}
		return nil
	}
}

func testAccUserMFAConfig(withMFA bool) string {
	cfg := fmt.Sprintf(`
resource "pcd_identity_user" "test" {
  name     = %q
  password = %q
}
`, testAccMFAUser, testAccMFAPassword)
	if withMFA {
		cfg += fmt.Sprintf(`
resource "pcd_identity_user_mfa" "test" {
  user_id     = pcd_identity_user.test.id
  totp_secret = %q
}
`, testAccMFASecret)
	}
	return cfg
}

// totpCode computes the RFC 6238 passcode Keystone expects: HMAC-SHA1 over the
// 30-second counter, six digits.
func totpCode(secret string, at time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(secret, "="))
	if err != nil {
		return "", err
	}
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg)
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", code), nil
}

// loginAsMFAUser authenticates as the test user, with a fresh passcode when
// withPasscode is set. It waits out the end of a 30-second window so the
// passcode is still valid when Keystone checks it.
func loginAsMFAUser(withPasscode bool) error {
	cfg := &clients.Config{
		AuthURL:      os.Getenv("OS_AUTH_URL"),
		Username:     testAccMFAUser,
		Password:     testAccMFAPassword,
		UserDomainID: "default",
		Insecure:     os.Getenv("OS_INSECURE") != "",
	}
	if withPasscode {
		if time.Now().Unix()%30 > 25 {
			time.Sleep(6 * time.Second)
		}
		code, err := totpCode(testAccMFASecret, time.Now())
		if err != nil {
			return err
		}
		cfg.Passcode = code
	}
	return cfg.Authenticate(context.Background())
}

func testAccCheckMFALogin(t *testing.T, withPasscode bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		err := loginAsMFAUser(withPasscode)
		if withPasscode && err != nil {
			return fmt.Errorf("login with password and passcode failed: %w", err)
		}
		if !withPasscode && err == nil {
			return fmt.Errorf("login with the password alone succeeded; MFA is not enforced (is totp in Keystone's [auth] methods?)")
		}
		return nil
	}
}

func testAccCheckPasswordOnlyLoginWorks(t *testing.T) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if err := loginAsMFAUser(false); err != nil {
			return fmt.Errorf("login with the password alone failed after MFA was removed: %w", err)
		}
		return nil
	}
}

func testAccCheckTOTPCredential(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		cred, err := clients.RequireObject(credentials.Get(context.Background(), client, rs.Primary.Attributes["credential_id"]).Extract())
		if err != nil {
			return fmt.Errorf("TOTP credential not found via API: %w", err)
		}
		if cred.Type != "totp" || cred.Blob != testAccMFASecret || cred.UserID != rs.Primary.Attributes["user_id"] {
			return fmt.Errorf("credential %s: type %q, user %q, blob matches the configured secret: %v; want a totp credential of user %s holding the configured secret", cred.ID, cred.Type, cred.UserID, cred.Blob == testAccMFASecret, rs.Primary.Attributes["user_id"])
		}
		return nil
	}
}

// testAccSetMFARules changes the test user's rules behind Terraform's back.
func testAccSetMFARules(t *testing.T, rules [][]string) {
	client, err := acctest.LabConfig(t).IdentityV3Client()
	if err != nil {
		t.Fatal(err)
	}
	pages, err := users.List(client, users.ListOpts{Name: testAccMFAUser, DomainID: "default"}).AllPages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	all, err := users.ExtractUsers(pages)
	if err != nil || len(all) != 1 {
		t.Fatalf("finding user %s: %d users, %v", testAccMFAUser, len(all), err)
	}
	if _, err := users.Update(context.Background(), client, all[0].ID, users.UpdateOpts{
		Options: map[users.Option]any{users.MultiFactorAuthRules: rules},
	}).Extract(); err != nil {
		t.Fatal(err)
	}
}

// testAccCheckNoMFARules checks that user n has neither MFA options nor a TOTP
// credential left. It runs while the user still exists: Keystone deletes a
// user's credentials together with the user, so only a check made before that
// proves that destroying pcd_identity_user_mfa deleted the credential.
func testAccCheckNoMFARules(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		// An empty ID would drop the user_id filter from the list below.
		if rs.Primary.ID == "" {
			return fmt.Errorf("%s has no ID in state", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		u, err := clients.RequireObject(users.Get(context.Background(), client, rs.Primary.ID).Extract())
		if err != nil {
			return err
		}
		for _, opt := range []users.Option{users.MultiFactorAuthRules, users.MultiFactorAuthEnabled} {
			if v, set := u.Options[string(opt)]; set {
				return fmt.Errorf("user %s still has %s = %v", rs.Primary.ID, opt, v)
			}
		}
		pages, err := credentials.List(client, credentials.ListOpts{UserID: rs.Primary.ID, Type: "totp"}).AllPages(context.Background())
		if err != nil {
			return err
		}
		left, err := credentials.ExtractCredentials(pages)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			return fmt.Errorf("user %s still has TOTP credential %s", rs.Primary.ID, left[0].ID)
		}
		return nil
	}
}

func testAccCheckUserMFADestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_user_mfa" {
				continue
			}
			_, err := credentials.Get(context.Background(), client, rs.Primary.Attributes["credential_id"]).Extract()
			if err == nil {
				return fmt.Errorf("TOTP credential %s still exists", rs.Primary.Attributes["credential_id"])
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking credential %s: %w", rs.Primary.Attributes["credential_id"], err)
			}
		}
		return nil
	}
}
