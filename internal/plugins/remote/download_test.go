package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/pluginmanifest"
)

// releaseServer answers like the real GitHub release API rather than like a
// fixture that says yes to anything.
//
// THIS SHAPE IS THE POINT. Unit 2 shipped five bugs behind a green suite
// because every test server returned the same body whatever was asked for, so
// the tests would have passed against a client that called entirely the wrong
// endpoint. Here the paths are the ones api.github.com actually serves,
// verified by hand against Infrena/infrena-provider-aws v0.4.0: a release is
// read at /repos/{owner}/{repo}/releases/tags/{tag}, and an asset's BYTES come
// from /repos/{owner}/{repo}/releases/assets/{id} with an octet-stream Accept.
// A request for anything else gets the 404 the real forge would give.
func releaseServer(t *testing.T, owner, repo, tag string, assets map[string][]byte) *httptest.Server {
	t.Helper()

	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}
	sort.Strings(names)
	ids := make(map[int]string, len(names))
	for i, name := range names {
		ids[i+1] = name
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == fmt.Sprintf("/repos/%s/%s/releases/tags/%s", owner, repo, tag):
			type asset struct {
				Name string `json:"name"`
				ID   int    `json:"id"`
				Size int    `json:"size"`
				URL  string `json:"url"`
			}
			out := struct {
				TagName string  `json:"tag_name"`
				Assets  []asset `json:"assets"`
			}{TagName: tag}
			for id := 1; id <= len(ids); id++ {
				name := ids[id]
				out.Assets = append(out.Assets, asset{
					Name: name,
					ID:   id,
					Size: len(assets[name]),
					URL:  fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", srv.URL, owner, repo, id),
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)

		case strings.HasPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/releases/assets/", owner, repo)):
			var id int
			if _, err := fmt.Sscanf(r.URL.Path, fmt.Sprintf("/repos/%s/%s/releases/assets/%%d", owner, repo), &id); err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			name, ok := ids[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// The real API serves the JSON description unless the caller asks
			// for the bytes, so a client that forgets the header gets metadata
			// where it expected an archive.
			if !strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"name":%q}`, name)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(assets[name])

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The convention mirrors infrena's own releases, verified against
// infrena_0.7.1_linux_amd64.tar.gz and
// infrena-plugin-aws_0.4.0_linux_amd64.tar.gz.
func TestAssetNameFollowsTheReleaseConvention(t *testing.T) {
	got := AssetName("infrena-plugin-aws", "0.4.0", pluginmanifest.Platform{OS: "linux", Arch: "amd64"})
	if got != "infrena-plugin-aws_0.4.0_linux_amd64.tar.gz" {
		t.Errorf("AssetName = %q", got)
	}
}

// A BACKEND IS NOT NAMED LIKE A PROVIDER, and this function used to assume it
// was: it pasted infrena-plugin- in front of whatever it was given, so a
// backend install asked for infrena-plugin-s3_1.0.0_linux_amd64.tar.gz and no
// release has ever published that. Verified against infrena-backend-s3's own
// scripts/build-release.
func TestAssetNameOfABackendIsNamedAfterTheBackendBinary(t *testing.T) {
	got := AssetName("infrena-backend-s3", "1.0.0", pluginmanifest.Platform{OS: "linux", Arch: "amd64"})
	if got != "infrena-backend-s3_1.0.0_linux_amd64.tar.gz" {
		t.Errorf("AssetName = %q", got)
	}
}

// Windows archives are ZIPs, not tarballs, in every real release. Asking for a
// .tar.gz that does not exist would come back as a 404, and a 404 on this path
// reads as "there is no build for your machine" - a plugin reported absent when
// it is merely named wrongly, which is the mistake section 31.3 forbids.
func TestAssetNameUsesZipOnWindows(t *testing.T) {
	got := AssetName("infrena-plugin-aws", "0.4.0", pluginmanifest.Platform{OS: "windows", Arch: "amd64"})
	if got != "infrena-plugin-aws_0.4.0_windows_amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func TestChecksumsParsesSHA256SUMS(t *testing.T) {
	// The ./ prefix is what sha256sum ./* actually writes, and what the real
	// SHA256SUMS of infrena-provider-aws v0.4.0 contains. A parser that keys on
	// the raw field finds no checksum for any asset and refuses every install.
	body := "aaaa  ./infrena-plugin-aws_0.4.0_linux_amd64.tar.gz\nbbbb  ./infrena-plugin-aws_0.4.0_darwin_arm64.tar.gz\n"
	srv := releaseServer(t, "infrena", "infrena-provider-aws", "v0.4.0", map[string][]byte{
		ChecksumsName: []byte(body),
	})
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, RawURL: srv.URL}

	got, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if got["infrena-plugin-aws_0.4.0_linux_amd64.tar.gz"] != "aaaa" {
		t.Errorf("Checksums = %v", got)
	}
	if got["infrena-plugin-aws_0.4.0_darwin_arm64.tar.gz"] != "bbbb" {
		t.Errorf("Checksums = %v", got)
	}
}

// A release with no SHA256SUMS cannot be installed, and must say that rather
// than install something unverified.
func TestAReleaseWithNoChecksumsIsRefusedNotInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, RawURL: srv.URL}

	_, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0")
	if err == nil {
		t.Fatal("a release with no SHA256SUMS was accepted")
	}
	if !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Errorf("error does not name what is missing: %v", err)
	}
}

// The release exists and its archives are there, but nobody published the sums.
// That is the more likely shape of the failure than a missing release, and it
// must refuse just as loudly rather than fall back to installing unverified.
func TestAReleaseWhoseAssetsLackSHA256SUMSIsRefused(t *testing.T) {
	srv := releaseServer(t, "infrena", "infrena-provider-aws", "v0.4.0", map[string][]byte{
		"infrena-plugin-aws_0.4.0_linux_amd64.tar.gz": []byte("archive"),
	})
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0")
	if err == nil {
		t.Fatal("a release publishing no checksums was accepted")
	}
	if !strings.Contains(err.Error(), ChecksumsName) {
		t.Errorf("error does not name what is missing: %v", err)
	}
}

// A file that is present but says nothing cannot vouch for anything, so it is
// refused rather than treated as "no checksum recorded for your archive".
func TestAnEmptyChecksumsFileIsRefused(t *testing.T) {
	srv := releaseServer(t, "infrena", "infrena-provider-aws", "v0.4.0", map[string][]byte{
		ChecksumsName: []byte("\n\n"),
	})
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	if _, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0"); err == nil {
		t.Fatal("an empty SHA256SUMS was accepted")
	}
}

// SECTION 31.3, ON THE DOWNLOAD PATH. Five bugs in Unit 2 were all this one
// mistake: a condition that means "I could not see" reported as "it is not
// there". A rate limit while fetching a release must stay a rate limit, or a
// user is sent to check a spelling that was right.
func TestARateLimitOnTheDownloadPathIsNotAMissingRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"Checksums", func() error {
			_, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0")
			return err
		}},
		{"ReleaseAsset", func() error {
			_, err := c.ReleaseAsset(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0", "any.tar.gz")
			return err
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			err := call.run()
			if err == nil {
				t.Fatal("a rate limit was accepted as a successful answer")
			}
			var limited *RateLimitError
			if !errors.As(err, &limited) {
				t.Fatalf("a rate limit was reported as %T: %v", err, err)
			}
			var missing *NotFoundError
			if errors.As(err, &missing) {
				t.Errorf("a rate limit also reads as not found: %v", err)
			}
		})
	}
}

func TestReleaseAssetReturnsTheBytes(t *testing.T) {
	want := "the archive bytes"
	srv := releaseServer(t, "infrena", "infrena-provider-aws", "v0.4.0", map[string][]byte{
		"infrena-plugin-aws_0.4.0_linux_amd64.tar.gz": []byte(want),
		ChecksumsName: []byte("aaaa  ./infrena-plugin-aws_0.4.0_linux_amd64.tar.gz\n"),
	})
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	got, err := c.ReleaseAsset(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0",
		"infrena-plugin-aws_0.4.0_linux_amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("ReleaseAsset = %q, want %q", got, want)
	}
}

// A release that exists but has no build for this machine names the asset it
// looked for, so the reader can see it is a missing platform rather than a
// missing plugin.
func TestAnAssetTheReleaseDoesNotHaveNamesWhatWasLookedFor(t *testing.T) {
	srv := releaseServer(t, "infrena", "infrena-provider-aws", "v0.4.0", map[string][]byte{
		"infrena-plugin-aws_0.4.0_linux_amd64.tar.gz": []byte("archive"),
	})
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.ReleaseAsset(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0",
		"infrena-plugin-aws_0.4.0_darwin_arm64.tar.gz")
	if err == nil {
		t.Fatal("an asset the release does not publish was accepted")
	}
	if !strings.Contains(err.Error(), "darwin_arm64") {
		t.Errorf("error does not name the asset: %v", err)
	}
	var missing *NotFoundError
	if !errors.As(err, &missing) {
		t.Errorf("a genuinely absent asset is %T rather than a NotFoundError: %v", err, err)
	}
}

// An unbounded download is the same hazard as an unbounded extraction: a forge
// answering forever fills memory before anything has been verified.
func TestAnOversizedAssetIsRefused(t *testing.T) {
	srv := releaseServer(t, "infrena", "infrena-provider-aws", "v0.4.0", map[string][]byte{
		"infrena-plugin-aws_0.4.0_linux_amd64.tar.gz": []byte(strings.Repeat("A", 4096)),
	})
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, MaxAssetBytes: 1024}

	_, err := c.ReleaseAsset(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0",
		"infrena-plugin-aws_0.4.0_linux_amd64.tar.gz")
	if err == nil {
		t.Fatal("an asset past the size limit was accepted")
	}
	if !strings.Contains(err.Error(), "1024") {
		t.Errorf("error does not say what the limit was: %v", err)
	}
}

// The bytes are fetched from the host infrena was configured with, NEVER from
// a URL the response body chose. A release listing is data from the forge, and
// following a download URL out of it would let whatever served the listing
// decide where an executable comes from.
func TestTheDownloadStaysOnTheConfiguredHost(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("attacker bytes"))
	}))
	defer elsewhere.Close()

	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/tags/v0.4.0"):
			fmt.Fprintf(w, `{"tag_name":"v0.4.0","assets":[{"name":%q,"id":7,"url":%q}]}`,
				"infrena-plugin-aws_0.4.0_linux_amd64.tar.gz", elsewhere.URL+"/evil")
		case strings.HasSuffix(r.URL.Path, "/releases/assets/7"):
			_, _ = w.Write([]byte("honest bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	got, err := c.ReleaseAsset(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0",
		"infrena-plugin-aws_0.4.0_linux_amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "honest bytes" {
		t.Errorf("the download followed the URL in the response body: %q", got)
	}
	if !contains(asked, "/repos/infrena/infrena-provider-aws/releases/assets/7") {
		t.Errorf("the asset was not read from the configured host: %v", asked)
	}
}

// The token is what makes a private publisher's release readable at all, and
// forgetting it here would make every private plugin look like it has no
// releases.
func TestTheTokenIsSentOnTheDownloadPath(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/tags/v0.4.0"):
			fmt.Fprintf(w, `{"tag_name":"v0.4.0","assets":[{"name":%q,"id":1}]}`, ChecksumsName)
		default:
			fmt.Fprint(w, "aaaa  ./infrena-plugin-aws_0.4.0_linux_amd64.tar.gz\n")
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "secret"}

	if _, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0"); err != nil {
		t.Fatal(err)
	}
	for i, got := range auth {
		if got != "Bearer secret" {
			t.Errorf("request %d sent Authorization %q", i, got)
		}
	}
	if len(auth) < 2 {
		t.Errorf("expected the release and the asset to be fetched, saw %d requests", len(auth))
	}
}

// A line that is not `<sum>  <name>` is refused rather than skipped. A skipped
// line is a checksum that silently stops existing, and an archive with no
// recorded checksum is exactly what this file exists to prevent.
func TestAMalformedChecksumsLineIsRefused(t *testing.T) {
	srv := releaseServer(t, "infrena", "infrena-provider-aws", "v0.4.0", map[string][]byte{
		ChecksumsName: []byte("aaaa  ./ok.tar.gz\nnonsense\n"),
	})
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.Checksums(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0")
	if err == nil {
		t.Fatal("a malformed SHA256SUMS line was accepted")
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("error does not quote the line it could not read: %v", err)
	}
}
