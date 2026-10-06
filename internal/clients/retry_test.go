// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
)

// These tests send real gophercloud calls through the provider's Neutron and
// Keystone clients to an offline fake, so the retry policy runs where
// gophercloud calls it, not on its own. Each route answers with a status the
// service, or an ingress in front of it, sends for that call.

const (
	secgroupsPath = "/v2.0/security-groups"
	secgroupPath  = secgroupsPath + "/sg-1"
	tokensPath    = "/v3/auth/tokens"
)

// secgroupJSON is Neutron's answer to a GET of sg-1.
const secgroupJSON = `{"security_group": {"id": "sg-1", "name": "workload-secgroup",
	"description": "", "stateful": true, "tenant_id": "proj-1", "project_id": "proj-1",
	"security_group_rules": []}}`

// fakeCloud is a test server that answers from its routes and logs every
// request it receives, like the networking tests' fakeNeutron. It serves one
// request at a time. A request no route names fails the test.
type fakeCloud struct {
	url string

	mu       sync.Mutex
	requests []string
}

// newFakeCloud starts a fakeCloud serving routes, each keyed "METHOD /path".
// The server closes when the test ends.
func newFakeCloud(t *testing.T, routes map[string]http.HandlerFunc) *fakeCloud {
	t.Helper()
	f := &fakeCloud{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, call)
		if handle, ok := routes[call]; ok {
			handle(w, r)
			return
		}
		// 501 is no status Neutron or Keystone sends for any call.
		t.Errorf("unexpected request %s", call)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// received returns the requests served so far, in order, each written
// "METHOD /path".
func (f *fakeCloud) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// reply answers with status and, unless body is empty, a JSON body.
func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// errorPage answers status with an HTML page like the one nginx serves for it.
// The body is not JSON.
func errorPage(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		title := fmt.Sprintf("%d %s", status, http.StatusText(status))
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<html>\r\n<head><title>%s</title></head>\r\n<body>\r\n"+
			"<center><h1>%s</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n", title, title)
	}
}

// badGateway answers 502 with nginx's HTML page.
var badGateway = errorPage(http.StatusBadGateway)

// dropConnection closes the connection without answering, so the client gets
// a transport error rather than a status.
func dropConnection(w http.ResponseWriter, _ *http.Request) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		conn.Close()
	}
}

// failingFirst answers the first request with fail and every later one with
// then: a call that fails once and works when it is sent again.
func failingFirst(fail, then http.HandlerFunc) http.HandlerFunc {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			fail(w, r)
			return
		}
		then(w, r)
	}
}

// keystoneToken answers a token request the way Keystone does: 201, the token
// in X-Subject-Token, and a catalog whose one entry is Neutron on this server.
func keystoneToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Subject-Token", "fake-token")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"token": {"methods": ["token"], "expires_at": "2026-10-03T00:00:00.000000Z",
		"project": {"id": "proj-1", "name": "service", "domain": {"id": "default", "name": "Default"}},
		"catalog": [{"id": "svc-1", "type": "network", "name": "neutron", "endpoints": [
			{"id": "ep-1", "interface": "public", "region": "region-one", "region_id": "region-one",
			 "url": "http://%s/"}]}]}}`, r.Host)
}

// neutronVersions answers the version discovery GET that gophercloud sends to
// a catalog endpoint before it builds a client for it, the way Neutron does.
func neutronVersions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"versions": [{"id": "v2.0", "status": "CURRENT",
		"links": [{"href": "http://%s/v2.0/", "rel": "self"}]}]}`, r.Host)
}

// waitLog stands in for the wait between retries. It records each delay it is
// asked to wait and returns at once, so the tests never sleep.
type waitLog []time.Duration

func (l *waitLog) sleep(_ context.Context, d time.Duration) error {
	*l = append(*l, d)
	return nil
}

// retryingConfig returns a Config whose clients reach the fake at url, with
// the retry policy max_retries installs, waiting through sleep. Like the
// networking tests' fakeConfig, it reaches the fake through EndpointLocator
// and leaves EndpointOverrides empty, so each client keeps its version prefix.
func retryingConfig(url string, maxRetries int, sleep func(context.Context, time.Duration) error) *Config {
	return &Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
			RetryFunc:       newRetryFunc(maxRetries, sleep),
		},
	}
}

// retryingNeutron returns the provider's Neutron client for the fake at url,
// built from retryingConfig.
func retryingNeutron(t *testing.T, url string, maxRetries int, sleep func(context.Context, time.Duration) error) *gophercloud.ServiceClient {
	t.Helper()
	client, err := retryingConfig(url, maxRetries, sleep).NetworkV2Client()
	if err != nil {
		t.Fatalf("building the Neutron client: %v", err)
	}
	return client
}

// A GET that draws a 429 or a 5xx is safe to send again, so it is retried
// after a wait, and the call succeeds when the retry does.
func TestRetryRetriesAGetThatDrewATransientStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			neutron := newFakeCloud(t, map[string]http.HandlerFunc{
				"GET " + secgroupPath: failingFirst(errorPage(status), reply(http.StatusOK, secgroupJSON)),
			})
			var waited waitLog

			sg, err := groups.Get(context.Background(), retryingNeutron(t, neutron.url, 2, waited.sleep), "sg-1").Extract()
			if err != nil {
				t.Fatalf("get returned %v; want the retry's answer", err)
			}
			if sg.ID != "sg-1" {
				t.Fatalf("get returned group %q, want sg-1", sg.ID)
			}
			want := []string{"GET " + secgroupPath, "GET " + secgroupPath}
			if sent := neutron.received(); !slices.Equal(sent, want) {
				t.Fatalf("sent %v, want %v", sent, want)
			}
			if !slices.Equal(waited, waitLog{time.Second}) {
				t.Fatalf("waited %v before retrying, want [1s]", waited)
			}
		})
	}
}

// A DELETE is retried the same way. If the first one went through after all,
// the retry usually draws a 404, which the resources' Delete treats as already
// gone; a service that deletes in the background may answer 409 or 400
// instead, which is no worse than the 502 the retry replaced.
func TestRetryRetriesADeleteThatDrewA502(t *testing.T) {
	t.Parallel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"DELETE " + secgroupPath: failingFirst(badGateway, reply(http.StatusNoContent, "")),
	})
	var waited waitLog

	if err := groups.Delete(context.Background(), retryingNeutron(t, neutron.url, 2, waited.sleep), "sg-1").ExtractErr(); err != nil {
		t.Fatalf("delete returned %v; want the retry's answer", err)
	}
	want := []string{"DELETE " + secgroupPath, "DELETE " + secgroupPath}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
	if !slices.Equal(waited, waitLog{time.Second}) {
		t.Fatalf("waited %v before retrying, want [1s]", waited)
	}
}

// A GET whose connection is dropped before any answer arrives is retried too.
func TestRetryRetriesAGetWhoseConnectionDropped(t *testing.T) {
	t.Parallel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"GET " + secgroupPath: failingFirst(dropConnection, reply(http.StatusOK, secgroupJSON)),
	})
	var waited waitLog

	sg, err := groups.Get(context.Background(), retryingNeutron(t, neutron.url, 2, waited.sleep), "sg-1").Extract()
	if err != nil {
		t.Fatalf("get returned %v; want the retry's answer", err)
	}
	if sg.ID != "sg-1" {
		t.Fatalf("get returned group %q, want sg-1", sg.ID)
	}
	want := []string{"GET " + secgroupPath, "GET " + secgroupPath}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
	if !slices.Equal(waited, waitLog{time.Second}) {
		t.Fatalf("waited %v before retrying, want [1s]", waited)
	}
}

// A POST is never retried. Neutron may have created the group before the 502
// came back, and a second POST would create another one.
func TestRetryNeverRetriesAPost(t *testing.T) {
	t.Parallel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"POST " + secgroupsPath: badGateway,
	})
	var waited waitLog

	_, err := groups.Create(context.Background(), retryingNeutron(t, neutron.url, 3, waited.sleep),
		groups.CreateOpts{Name: "workload-secgroup"}).Extract()
	if !gophercloud.ResponseCodeIs(err, http.StatusBadGateway) {
		t.Fatalf("create returned %v; want the 502", err)
	}
	if sent, want := neutron.received(), []string{"POST " + secgroupsPath}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want exactly one POST", sent)
	}
	if len(waited) != 0 {
		t.Fatalf("waited %v; a POST must fail without waiting", waited)
	}
}

// A PUT is never retried either: several OpenStack PUTs, such as
// add_router_interface, are not idempotent.
func TestRetryNeverRetriesAPut(t *testing.T) {
	t.Parallel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"PUT " + secgroupPath: badGateway,
	})
	var waited waitLog

	_, err := groups.Update(context.Background(), retryingNeutron(t, neutron.url, 3, waited.sleep), "sg-1",
		groups.UpdateOpts{Name: "renamed-secgroup"}).Extract()
	if !gophercloud.ResponseCodeIs(err, http.StatusBadGateway) {
		t.Fatalf("update returned %v; want the 502", err)
	}
	if sent, want := neutron.received(), []string{"PUT " + secgroupPath}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want exactly one PUT", sent)
	}
	if len(waited) != 0 {
		t.Fatalf("waited %v; a PUT must fail without waiting", waited)
	}
}

// Nor is a PATCH, such as the one pcd_identity_project sends to Keystone to
// update a project.
func TestRetryNeverRetriesAPatch(t *testing.T) {
	t.Parallel()
	keystone := newFakeCloud(t, map[string]http.HandlerFunc{
		"PATCH /v3/projects/proj-1": badGateway,
	})
	var waited waitLog
	client, err := retryingConfig(keystone.url, 3, waited.sleep).IdentityV3Client()
	if err != nil {
		t.Fatalf("building the Keystone client: %v", err)
	}

	_, err = projects.Update(context.Background(), client, "proj-1", projects.UpdateOpts{Name: "renamed-project"}).Extract()
	if !gophercloud.ResponseCodeIs(err, http.StatusBadGateway) {
		t.Fatalf("update returned %v; want the 502", err)
	}
	if sent, want := keystone.received(), []string{"PATCH /v3/projects/proj-1"}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want exactly one PATCH", sent)
	}
	if len(waited) != 0 {
		t.Fatalf("waited %v; a PATCH must fail without waiting", waited)
	}
}

// A 404 is an answer, not a transient failure, so it is returned at once for
// the resource's not-found handling.
func TestRetryDoesNotRetryA404(t *testing.T) {
	t.Parallel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"GET " + secgroupPath: reply(http.StatusNotFound,
			`{"NeutronError": {"type": "SecurityGroupNotFound", "message": "Security group sg-1 does not exist", "detail": ""}}`),
	})
	var waited waitLog

	_, err := groups.Get(context.Background(), retryingNeutron(t, neutron.url, 3, waited.sleep), "sg-1").Extract()
	if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		t.Fatalf("get returned %v; want the 404", err)
	}
	if sent, want := neutron.received(), []string{"GET " + secgroupPath}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want exactly one GET", sent)
	}
	if len(waited) != 0 {
		t.Fatalf("waited %v; a 404 must be returned without waiting", waited)
	}
}

// A 200 whose body is not JSON, such as a page a misbehaving proxy serves, is
// neither a transient status nor a lost answer. gophercloud hands the decode
// error to the RetryFunc too, and it is returned at once.
func TestRetryDoesNotRetryAnAnswerThatIsNotJSON(t *testing.T) {
	t.Parallel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"GET " + secgroupPath: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>Welcome</body></html>")
		},
	})
	var waited waitLog

	_, err := groups.Get(context.Background(), retryingNeutron(t, neutron.url, 3, waited.sleep), "sg-1").Extract()
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("get returned %v; want the decode error", err)
	}
	if sent, want := neutron.received(), []string{"GET " + secgroupPath}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want exactly one GET", sent)
	}
	if len(waited) != 0 {
		t.Fatalf("waited %v; a decode error must be returned without waiting", waited)
	}
}

// A GET that keeps failing is sent max_retries + 1 times in all, and then the
// last error is returned unchanged. Each wait is twice the one before, up to
// 30 seconds.
func TestRetryGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"GET " + secgroupPath: badGateway,
	})
	var waited waitLog

	_, err := groups.Get(context.Background(), retryingNeutron(t, neutron.url, 6, waited.sleep), "sg-1").Extract()
	if !gophercloud.ResponseCodeIs(err, http.StatusBadGateway) {
		t.Fatalf("get returned %v; want the last 502", err)
	}
	if sent := neutron.received(); len(sent) != 7 {
		t.Fatalf("sent %d GETs (%v), want 7: the first and 6 retries", len(sent), sent)
	}
	want := waitLog{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	if !slices.Equal(waited, want) {
		t.Fatalf("waited %v, want %v", waited, want)
	}
}

// The wait between retries ends as soon as the request's context does. The
// call then returns the context's error instead of sleeping out the delay,
// and the error still says what the last attempt drew.
func TestRetryStopsWaitingWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"GET " + secgroupPath: badGateway,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The real wait, with the context ending while it runs.
	sleep := func(ctx context.Context, d time.Duration) error {
		go cancel()
		return sleepContext(ctx, d)
	}

	start := time.Now()
	_, err := groups.Get(ctx, retryingNeutron(t, neutron.url, 3, sleep), "sg-1").Extract()
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("get returned %v; want the context's error", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("get returned %v; want it to keep the 502 the last attempt drew", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("get returned after %s; want it back before the 1s wait would have ended", elapsed)
	}
	if sent, want := neutron.received(), []string{"GET " + secgroupPath}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want exactly one GET", sent)
	}
}

// A request whose context ends while it is in flight is not retried. Its
// transport error comes from the context, so the call returns it without
// waiting.
func TestRetryDoesNotRetryARequestWhoseContextEnded(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	neutron := newFakeCloud(t, map[string]http.HandlerFunc{
		"GET " + secgroupPath: func(_ http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done() // the client hangs up
		},
	})
	var waited waitLog

	_, err := groups.Get(ctx, retryingNeutron(t, neutron.url, 3, waited.sleep), "sg-1").Extract()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("get returned %v; want the context's error", err)
	}
	if len(waited) != 0 {
		t.Fatalf("waited %v; a request whose context ended must not be retried", waited)
	}
	if sent, want := neutron.received(), []string{"GET " + secgroupPath}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want exactly one GET", sent)
	}
}

// max_retries defaults to 0, which keeps gophercloud's own behavior.
// Authenticate installs no RetryFunc, and a GET that draws a 502 fails on the
// first answer.
func TestAuthenticateInstallsNoRetryFuncByDefault(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t, map[string]http.HandlerFunc{
		"POST " + tokensPath:  keystoneToken,
		"GET /":               neutronVersions,
		"GET " + secgroupPath: badGateway,
	})
	cfg := &Config{AuthURL: cloud.url + "/v3", Region: "region-one", Token: "fake-token", TenantID: "proj-1"}
	if err := cfg.Authenticate(context.Background()); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if cfg.Provider.RetryFunc != nil {
		t.Fatal("Authenticate installed a RetryFunc with max_retries unset")
	}
	client, err := cfg.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the Neutron client: %v", err)
	}

	_, err = groups.Get(context.Background(), client, "sg-1").Extract()
	if !gophercloud.ResponseCodeIs(err, http.StatusBadGateway) {
		t.Fatalf("get returned %v; want the 502", err)
	}
	if sent, want := cloud.received(), []string{"POST " + tokensPath, "GET /", "GET " + secgroupPath}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}

// With max_retries set, Authenticate installs the retry policy with its real
// wait. A GET that draws a 502 then waits to be retried, and the wait ends
// when the request's context does instead of running out its full second.
func TestAuthenticateInstallsTheRetryFuncWhenMaxRetriesIsSet(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cloud := newFakeCloud(t, map[string]http.HandlerFunc{
		"POST " + tokensPath: keystoneToken,
		"GET /":              neutronVersions,
		// The context ends shortly after the 502 goes out, while the client
		// waits to retry.
		"GET " + secgroupPath: func(w http.ResponseWriter, r *http.Request) {
			badGateway(w, r)
			time.AfterFunc(50*time.Millisecond, cancel)
		},
	})
	cfg := &Config{AuthURL: cloud.url + "/v3", Region: "region-one", Token: "fake-token", TenantID: "proj-1", MaxRetries: 3}
	if err := cfg.Authenticate(context.Background()); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if cfg.Provider.RetryFunc == nil {
		t.Fatal("Authenticate installed no RetryFunc with max_retries = 3")
	}
	client, err := cfg.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the Neutron client: %v", err)
	}

	start := time.Now()
	_, err = groups.Get(ctx, client, "sg-1").Extract()
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("get returned %v; want the context's error, which ended the wait to retry", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("get returned after %s; want it back before the 1s wait would have ended", elapsed)
	}
	if sent, want := cloud.received(), []string{"POST " + tokensPath, "GET /", "GET " + secgroupPath}; !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}

// A GET to a server whose certificate the provider does not trust fails the
// same way every time, so it is not retried: waiting would only delay the
// error by up to a minute or more.
func TestRetryDoesNotRetryAnUntrustedCertificate(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	handshakes := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the request reached the handler; the TLS handshake should have failed")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			handshakes++
			mu.Unlock()
		}
	}
	// The failed handshakes are the point of the test; keep them out of its log.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	var waited waitLog

	// retryingNeutron's client trusts only the system roots, which do not
	// include the test server's certificate.
	_, err := groups.Get(context.Background(), retryingNeutron(t, srv.URL, 2, waited.sleep), "sg-1").Extract()
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) {
		t.Fatalf("get returned %v; want a certificate verification error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if handshakes != 1 {
		t.Fatalf("connected %d times, want 1: an untrusted certificate must not be retried", handshakes)
	}
	if len(waited) != 0 {
		t.Fatalf("waited %v, want no wait", waited)
	}
}

// Which transport errors are retried: a connection that was refused, reset or
// timed out may work a moment later, but a certificate the provider does not
// trust or a host name that does not resolve will not, so those fail at once.
// A DNS lookup that timed out may work when sent again. The errors are built
// the way http.Client wraps them, since an offline test cannot produce a real
// DNS failure reliably.
func TestRetryRetriesOnlyTransportErrorsThatWaitingCanFix(t *testing.T) {
	t.Parallel()
	dial := func(err error) error {
		return &url.Error{Op: "Get", URL: "https://pcd.example/neutron/v2.0/security-groups/sg-1",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: err}}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", dial(os.NewSyscallError("connect", syscall.ECONNREFUSED)), true},
		{"connection reset", &url.Error{Op: "Get", URL: "https://pcd.example/", Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}, true},
		{"dns timeout", dial(&net.DNSError{Err: "i/o timeout", Name: "pcd.example", IsTimeout: true}), true},
		{"dns temporary failure", dial(&net.DNSError{Err: "server misbehaving", Name: "pcd.example", IsTemporary: true}), true},
		{"dns no such host", dial(&net.DNSError{Err: "no such host", Name: "pcd.example", IsNotFound: true}), false},
		{"untrusted certificate", &url.Error{Op: "Get", URL: "https://pcd.example/",
			Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, false},
		{"tls to a port that speaks neither tls nor http", &url.Error{Op: "Get", URL: "https://pcd.example/", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isRetryable(context.Background(), http.MethodGet, tc.err); got != tc.want {
				t.Fatalf("isRetryable(GET, %v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// An https:// endpoint that reaches a plain-HTTP port fails the same way every
// time: net/http reports it as http.ErrSchemeMismatch, not as a TLS error, so
// it is not retried either.
func TestRetryDoesNotRetryHTTPSToAPlainHTTPPort(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	connections := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the request reached the handler; the client should have refused the plain-HTTP answer")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			connections++
			mu.Unlock()
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	var waited waitLog

	httpsURL := "https://" + strings.TrimPrefix(srv.URL, "http://")
	_, err := groups.Get(context.Background(), retryingNeutron(t, httpsURL, 2, waited.sleep), "sg-1").Extract()
	if !errors.Is(err, http.ErrSchemeMismatch) {
		t.Fatalf("get returned %v; want http.ErrSchemeMismatch", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if connections != 1 {
		t.Fatalf("connected %d times, want 1: a scheme mismatch must not be retried", connections)
	}
	if len(waited) != 0 {
		t.Fatalf("waited %v, want no wait", waited)
	}
}

// A HEAD is as safe to send again as a GET, so it is retried on the same
// answers. No provider call sends HEAD today, so this checks the policy
// directly.
func TestRetryRetriesAHeadLikeAGet(t *testing.T) {
	t.Parallel()
	err := gophercloud.ErrUnexpectedResponseCode{Method: http.MethodHead, Actual: http.StatusBadGateway}
	if !isRetryable(context.Background(), http.MethodHead, err) {
		t.Fatal("isRetryable(HEAD, 502) = false, want true")
	}
}
