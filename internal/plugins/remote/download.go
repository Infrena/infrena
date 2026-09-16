package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/infrena/infrena/pkg/pluginmanifest"
)

const (
	// ChecksumsName is the file a release publishes beside its archives, and
	// the ONLY place a checksum is taken from.
	//
	// NOT THE MANIFEST (PLAN.md section 31.2). The manifest is committed before
	// the archives exist, so it cannot contain their hashes; a checksum that
	// travelled with the thing it vouches for would vouch for nothing anyway.
	// .github/workflows/release.yml writes this file with `sha256sum ./*` over
	// the whole dist directory.
	ChecksumsName = "SHA256SUMS"

	// binaryPrefix is the prefix on a plugin's executable, and therefore on the
	// archive that ships it.
	binaryPrefix = "infrena-plugin-"

	// maxAsset bounds a release archive.
	//
	// The real ones are around nine megabytes: infrena-plugin-aws 0.4.0 ships
	// between 8.2 and 9.2 MB per platform. A quarter of a gigabyte leaves room
	// for a plugin an order of magnitude larger while still refusing a forge
	// that answers forever, which would otherwise fill memory before a single
	// byte had been verified.
	maxAsset = 256 << 20
)

// AssetName is the release asset a plugin publishes for one platform.
//
// VERIFIED AGAINST REAL RELEASES, not inferred: infrena's own
// infrena_0.7.1_linux_amd64.tar.gz and the plugin's
// infrena-plugin-aws_0.4.0_linux_amd64.tar.gz, both produced by
// .github/workflows/release.yml, which names each archive
// <binary>_<version>_<goos>_<goarch>. The version carries NO leading v even
// though the tag does, because the workflow strips it.
//
// WINDOWS ARCHIVES ARE ZIPS. The same workflow zips the windows builds and
// tars everything else, so asking for a .tar.gz on windows would be a request
// for an asset that does not exist - and a 404 on this path reads as "this
// plugin publishes no build for your machine", which is section 31.3's
// forbidden "could not see it" rendered as "it is not there". Extraction
// handles tarballs only today, so a windows install fails at extraction with
// something true rather than at download with something false.
func AssetName(plugin, version string, p pluginmanifest.Platform) string {
	ext := ".tar.gz"
	if p.OS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s%s_%s_%s_%s%s", binaryPrefix, plugin, version, p.OS, p.Arch, ext)
}

// Checksums reads a release's SHA256SUMS, keyed by asset name.
//
// A RELEASE WITHOUT ONE CANNOT BE INSTALLED. Every error from here names
// SHA256SUMS, because the caller's only correct response is to refuse: there is
// no fallback in which an archive nothing can vouch for is installed anyway.
// Naming the file is also what keeps the refusal actionable, since the fix is
// for the publisher to ship it.
//
// The wrapping preserves the error underneath, so a rate limit is still a
// *RateLimitError to errors.As. That matters more here than anywhere: a forge
// that would not answer must never be reported as a publisher who shipped no
// checksums, which would send a user to open an issue against a release that is
// perfectly fine.
func (c *Client) Checksums(ctx context.Context, owner, repo, tag string) (map[string]string, error) {
	body, err := c.ReleaseAsset(ctx, owner, repo, tag, ChecksumsName)
	if err != nil {
		return nil, fmt.Errorf("reading %s for %s/%s at %s: %w.\n\nSuggested action:\n  A release with no %s cannot be installed, because nothing can vouch for the archive. Ask the publisher to publish one, or install a release that has one.",
			ChecksumsName, owner, repo, tag, err, ChecksumsName)
	}

	sums, err := parseChecksums(string(body))
	if err != nil {
		return nil, fmt.Errorf("the %s of %s/%s at %s is not usable: %w", ChecksumsName, owner, repo, tag, err)
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("the %s of %s/%s at %s is empty.\n\nA checksums file that lists nothing vouches for nothing, so it is refused rather than read as \"no checksum recorded for this archive\".\n\nSuggested action:\n  Ask the publisher to republish the release.",
			ChecksumsName, owner, repo, tag)
	}
	return sums, nil
}

// parseChecksums reads `sha256sum` output: a hex digest, whitespace, and a name.
//
// THE ./ PREFIX IS REAL AND IS STRIPPED. release.yml runs `sha256sum ./*`, so
// every line of a genuine SHA256SUMS names `./infrena-plugin-aws_0.4.0_…` and a
// parser keying on the raw field finds a checksum for no asset at all and
// refuses every install. A `*` marks binary mode and is stripped for the same
// reason.
//
// A LINE THAT DOES NOT PARSE FAILS THE FILE. Skipping it would make a checksum
// quietly stop existing, and an archive with no recorded checksum is the exact
// thing this file exists to prevent.
func parseChecksums(body string) (map[string]string, error) {
	sums := map[string]string{}
	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("line %d, %q, is not a checksum and a file name: expected `<sha256>  <name>`, which is what sha256sum writes", i+1, line)
		}
		name = strings.TrimSpace(name)
		name = strings.TrimPrefix(name, "*")
		name = strings.TrimPrefix(name, "./")
		if sum == "" || name == "" {
			return nil, fmt.Errorf("line %d, %q, is not a checksum and a file name: expected `<sha256>  <name>`, which is what sha256sum writes", i+1, line)
		}
		sums[name] = sum
	}
	return sums, nil
}

// ReleaseAsset downloads one asset of a release by name.
//
// TWO REQUESTS, THROUGH THE API, AND THERE IS NO SHORTER PATH. GitHub has no
// endpoint that addresses an asset by name, so the release is read at
// /repos/{owner}/{repo}/releases/tags/{tag} to find the asset's id, and the
// bytes come from /repos/{owner}/{repo}/releases/assets/{id} with an
// octet-stream Accept. The obvious one-request alternative,
// github.com/{owner}/{repo}/releases/download/{tag}/{asset}, was tried against
// the real forge and answers 404 for a private repository EVEN WITH A VALID
// BEARER TOKEN - so it would report every private publisher's plugin as
// nonexistent, which is precisely the failure section 31.3 forbids.
//
// THE ID IS TAKEN FROM THE LISTING; THE HOST IS NOT. The download URL is built
// from the configured BaseURL rather than from the `url` field of the response,
// so whatever served the listing cannot nominate somewhere else for an
// executable to come from.
//
// This downloads bytes. It does not verify them and it does not write them
// anywhere: verification is the caller's, against Checksums, before anything
// is opened.
func (c *Client) ReleaseAsset(ctx context.Context, owner, repo, tag, asset string) ([]byte, error) {
	id, err := c.assetID(ctx, owner, repo, tag, asset)
	if err != nil {
		return nil, err
	}

	what := fmt.Sprintf("the release asset %s of %s/%s at %s", asset, owner, repo, tag)
	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d",
		strings.TrimSuffix(c.base(), "/"), url.PathEscape(owner), url.PathEscape(repo), id)

	return c.fetch(ctx, endpoint, what, "application/octet-stream", c.maxAsset())
}

// assetID resolves an asset name to the id the download endpoint takes.
//
// An asset the release does not publish is a *NotFoundError naming the asset,
// so "there is no build for your machine" stays distinguishable from "the
// release could not be read" - the refusals from c.fetch below travel out
// unchanged for exactly that reason.
func (c *Client) assetID(ctx context.Context, owner, repo, tag, asset string) (int64, error) {
	what := fmt.Sprintf("the release %s of %s/%s", tag, owner, repo)
	endpoint := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s",
		strings.TrimSuffix(c.base(), "/"), url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(tag))

	body, err := c.fetch(ctx, endpoint, what, "application/vnd.github+json", maxBody)
	if err != nil {
		return 0, err
	}

	var release struct {
		Assets []struct {
			Name string `json:"name"`
			ID   int64  `json:"id"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return 0, fmt.Errorf("reading %s from %s: %w: expected a JSON release", what, endpoint, err)
	}

	published := make([]string, 0, len(release.Assets))
	for _, a := range release.Assets {
		if a.Name == asset {
			return a.ID, nil
		}
		published = append(published, a.Name)
	}
	return 0, &NotFoundError{What: fmt.Sprintf("%s (it publishes %s)", what+", asset "+asset, describeAssets(published))}
}

// describeAssets lists what the release does publish, so a missing platform
// reads as a missing platform rather than as a missing plugin.
func describeAssets(names []string) string {
	if len(names) == 0 {
		return "no assets at all"
	}
	return strings.Join(names, ", ")
}

// fetch performs one request and returns at most limit bytes, REFUSING rather
// than truncating when there are more.
//
// A truncated download is worse than a refused one: it would fail its checksum
// and be reported as a tampered archive, sending a reader to accuse a publisher
// of something the reader's own size limit did.
//
// It reuses classify, which is the whole reason this is not a second HTTP
// client. A rate limit, a forbidden and a genuine 404 must stay three different
// errors on the download path exactly as they are on the search path.
func (c *Client) fetch(ctx context.Context, endpoint, what, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building a request for %s: %w: %q is not a usable URL", what, err, endpoint)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w: check the network connection, or try again", what, err)
	}
	defer resp.Body.Close()

	if err := c.classify(resp, what); err != nil {
		return nil, err
	}

	// limit+1 so that a body of exactly limit bytes is not mistaken for the
	// truncation of something larger.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading the body of %s: %w: the response ended early, so try again", what, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d byte limit infrena will download.\n\nA download with no limit fills memory before anything has been verified, so it stops rather than continuing.\n\nSuggested action:\n  Check that this is the release you meant to install. Nothing has been installed.",
			what, limit)
	}
	return body, nil
}

// maxAsset is the download cap, overridable per client so the limit itself is
// testable without moving a quarter of a gigabyte through a test server.
func (c *Client) maxAsset() int64 {
	if c.MaxAssetBytes > 0 {
		return c.MaxAssetBytes
	}
	return maxAsset
}
