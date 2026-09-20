// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
)

// imageServer answers GET /images/<id> with each body in turn, repeating the
// last one once the list runs out, and counts the requests it saw so a test can
// assert both how often the wait polled and whether it cleaned up. The counters
// are atomic because the handler runs on the server's goroutine and the test
// reads them from its own.
type imageServer struct {
	bodies  []string
	gets    atomic.Int64
	deletes atomic.Int64
}

func newImageClient(t *testing.T, bodies ...string) (*gophercloud.ServiceClient, *imageServer) {
	t.Helper()
	s := &imageServer{bodies: bodies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			s.deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		i := int(s.gets.Add(1)) - 1
		if i >= len(s.bodies) {
			i = len(s.bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.bodies[i]))
	}))
	t.Cleanup(srv.Close)
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       srv.URL + "/",
	}, s
}

// imageBody renders a Glance image record. Keys in extra are plain response
// fields, which is how the os_glance_* bookkeeping reaches Image.Properties.
func imageBody(status string, extra map[string]string) string {
	fields := []string{
		`"id":"img-1"`,
		`"name":"tf-test"`,
		`"container_format":"bare"`,
		`"disk_format":"qcow2"`,
		`"tags":[]`,
		fmt.Sprintf(`"status":%q`, status),
	}
	for k, v := range extra {
		fields = append(fields, fmt.Sprintf("%q:%q", k, v))
	}
	return "{" + strings.Join(fields, ",") + "}"
}

// A web-download import that Glance could not complete leaves the image queued,
// not killed. Before this check the wait polled that queued image for the full
// 30 minutes and then reported only a timeout.
func TestWaitForImageActiveFailsFastOnAFailedImport(t *testing.T) {
	client, srv := newImageClient(t, imageBody("queued", map[string]string{
		"os_glance_failed_import": "file",
	}))

	start := time.Now()
	_, err := waitForImageActive(context.Background(), client, "img-1", 30*time.Minute)
	if err == nil {
		t.Fatal("got no error; the apply would keep polling a dead import")
	}
	if !errors.Is(err, errImportFailed) {
		t.Fatalf("errors.Is(%v, errImportFailed) = false; Create would not clean up the image", err)
	}
	if got := srv.gets.Load(); got != 1 {
		t.Fatalf("polled %d times, want 1: the failure is on the first read", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s; failing fast is the point of the change", elapsed)
	}
	for _, want := range []string{"img-1", "file", `"queued"`, "glance-api"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q, which the user needs to act on it", err, want)
		}
	}
}

// Every failed store has to be named, not just the first.
func TestWaitForImageActiveNamesEveryFailedStore(t *testing.T) {
	client, _ := newImageClient(t, imageBody("queued", map[string]string{
		"os_glance_failed_import": "file,ceph",
	}))

	_, err := waitForImageActive(context.Background(), client, "img-1", 30*time.Minute)
	if err == nil {
		t.Fatal("got no error")
	}
	if !strings.Contains(err.Error(), "file,ceph") {
		t.Fatalf("error %q drops one of the failed stores", err)
	}
}

// Glance's merge_store_list writes the key back as "" once every store has been
// removed from the list, so a present-but-empty value is not a failure. Reading
// it as one would break every import on a cloud that had ever cleared the list.
func TestWaitForImageActiveTreatsAnEmptyFailedImportAsNoFailure(t *testing.T) {
	client, _ := newImageClient(t, imageBody("queued", map[string]string{
		"os_glance_failed_import": "",
	}))

	_, err := waitForImageActive(context.Background(), client, "img-1", 0)
	if err == nil {
		t.Fatal("got no error at a zero timeout")
	}
	if errors.Is(err, errImportFailed) {
		t.Fatalf("empty os_glance_failed_import reported as a failure: %v", err)
	}
}

// A value the provider cannot read as a store list must not be stringified into
// the message or mistaken for a failure.
func TestWaitForImageActiveIgnoresANonStringFailedImport(t *testing.T) {
	client, _ := newImageClient(t, `{"id":"img-1","name":"tf-test","container_format":"bare",`+
		`"disk_format":"qcow2","tags":[],"status":"queued","os_glance_failed_import":42}`)

	_, err := waitForImageActive(context.Background(), client, "img-1", 0)
	if err == nil {
		t.Fatal("got no error at a zero timeout")
	}
	if errors.Is(err, errImportFailed) {
		t.Fatalf("a non-string value read as a failed store list: %v", err)
	}
	if strings.Contains(err.Error(), "42") {
		t.Fatalf("error %q stringified a value that is not a store list", err)
	}
}

// A multi-store import can lose one store and still bring the image to active.
// An active image is usable, so the status check has to win over the property.
func TestWaitForImageActiveSucceedsWhenOnlyOneStoreFailed(t *testing.T) {
	client, _ := newImageClient(t, imageBody("active", map[string]string{
		"os_glance_failed_import": "ceph",
	}))

	img, err := waitForImageActive(context.Background(), client, "img-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.Status != images.ImageStatusActive {
		t.Fatalf("status = %q, want active", img.Status)
	}
}

// killed keeps its own error and must stay distinguishable: Task 3 deletes the
// image for an import failure and deliberately leaves a killed one alone.
func TestWaitForImageActiveKilledIsNotAnImportFailure(t *testing.T) {
	client, _ := newImageClient(t, imageBody("killed", nil))

	_, err := waitForImageActive(context.Background(), client, "img-1", 30*time.Minute)
	if err == nil {
		t.Fatal("got no error for a killed image")
	}
	if errors.Is(err, errImportFailed) {
		t.Fatalf("killed reported as an import failure: %v", err)
	}
	if !strings.Contains(err.Error(), "killed") {
		t.Fatalf("error %q does not mention the killed state", err)
	}
}

// A timeout that says nothing but the status cannot be acted on. Glance keeps
// the stores an import is working on in os_glance_importing_to_stores, which
// separates a slow import from a wedged one.
func TestWaitForImageActiveTimeoutReportsImportProgress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{
			name: "nothing in flight",
			want: `timed out waiting for image img-1 to become active (last status "queued")`,
		},
		{
			name:  "still importing",
			extra: map[string]string{"os_glance_importing_to_stores": "file"},
			want:  `(last status "queued", still importing into store(s) file)`,
		},
		{
			name:  "empty importing list reads as nothing in flight",
			extra: map[string]string{"os_glance_importing_to_stores": ""},
			want:  `timed out waiting for image img-1 to become active (last status "queued")`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, srv := newImageClient(t, imageBody("queued", tc.extra))

			_, err := waitForImageActive(context.Background(), client, "img-1", 0)
			if err == nil {
				t.Fatal("got no error at a zero timeout")
			}
			if errors.Is(err, errImportFailed) {
				t.Fatalf("a timeout reported as an import failure: Create would delete an image that may still be importing: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if got := srv.gets.Load(); got != 1 {
				t.Fatalf("polled %d times at a zero timeout, want 1", got)
			}
		})
	}
}

// Create never records a failed image in state, so one left in Glance is an
// orphan that blocks the retry with a name or checksum clash. A timeout is a
// different matter: the import may still finish, and a killed image is the
// upload path's existing behavior, which this change does not touch.
func TestWaitForNewImageDeletesOnlyAFailedImport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		timeout    time.Duration
		wantDelete int64
	}{
		{
			name:       "failed import is deleted",
			body:       imageBody("queued", map[string]string{"os_glance_failed_import": "file"}),
			timeout:    30 * time.Minute,
			wantDelete: 1,
		},
		{
			name:    "killed is left alone",
			body:    imageBody("killed", nil),
			timeout: 30 * time.Minute,
		},
		{
			name:    "timeout is left alone",
			body:    imageBody("queued", nil),
			timeout: 0,
		},
		{
			name:    "active is left alone",
			body:    imageBody("active", nil),
			timeout: 30 * time.Minute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, srv := newImageClient(t, tc.body)

			_, _ = waitForNewImage(context.Background(), client, "img-1", tc.timeout)

			if got := srv.deletes.Load(); got != tc.wantDelete {
				t.Fatalf("deleted %d times, want %d", got, tc.wantDelete)
			}
		})
	}
}

// The successful path still hands the image back to Create.
func TestWaitForNewImageReturnsTheActiveImage(t *testing.T) {
	client, _ := newImageClient(t, imageBody("active", nil))

	img, err := waitForNewImage(context.Background(), client, "img-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img == nil || img.ID != "img-1" {
		t.Fatalf("img = %+v, want the image Create will flatten into state", img)
	}
}
