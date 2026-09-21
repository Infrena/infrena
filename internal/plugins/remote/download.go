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
	// the only place a checksum is taken from.
	//
	// Not the manifest: it is committed before the archives exist, so it cannot
	// contain their hashes, and a checksum travelling with the thing it vouches
	// for would vouch for nothing anyway.
	ChecksumsName = "SHA256SUMS"

	// maxAsset bounds a release archive. Real ones are around nine megabytes, so
	// a quarter of a gigabyte leaves room for a plugin an order of magnitude
	// larger while still refusing a forge that answers forever and would fill
	// memory before a single byte had been verified.
	maxAsset = 256 << 20
)

// AssetName is the release asset a plugin publishes for one platform.
//
// The shape is <binary>_<version>_<goos>_<goarch>, with no leading v on the
// version even though the tag carries one. It takes the binary name rather than
// the plugin name, because the archive is named after the binary inside it and
// the two differ by role: a provider ships `infrena-plugin-<name>` while a
// backend ships `infrena-backend-<name>`.
//
// Windows archives are zips and everything else is a tarball, so asking for a
// .tar.gz on windows would 404 — and a 404 here reads as "this plugin publishes
// no build for your machine", turning "could not see it" into "it is not there".
// The extractor decides the format from the bytes rather than from this name.
// The binary is spelled without the .exe a windows build carries: the extension
// belongs to the file inside the archive.
func AssetName(binary, version string, p pluginmanifest.Platform) string {
	ext := ".tar.gz"
	if p.OS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s%s", binary, version, p.OS, p.Arch, ext)
}

// Checksums reads a release's SHA256SUMS, keyed by asset name.
//
// A release without one cannot be installed: there is no fallback in which an
// archive nothing can vouch for is installed anyway. Every error from here names
// the file, since the fix is for the publisher to ship it.
//
// The wrapping preserves the error underneath, so a rate limit is still a
// *RateLimitError to errors.As. A forge that would not answer must never be
// reported as a publisher who shipped no checksums.
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
// The `./` prefix is real and is stripped: releases are hashed with
// `sha256sum ./*`, so keying on the raw field would find a checksum for no asset
// at all and refuse every install. A leading `*`, which marks binary mode, is
// stripped for the same reason.
//
// A line that does not parse fails the file. Skipping it would make a checksum
// quietly stop existing, and an archive with no recorded checksum is what this
// file exists to prevent.
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
// Two requests, because GitHub has no endpoint addressing an asset by name: the
// release is read at its tag to find the asset's id, then the bytes are fetched
// by id. The one-request alternative,
// github.com/{owner}/{repo}/releases/download/{tag}/{asset}, answers 404 for a
// private repository even with a valid bearer token, so it would report every
// private publisher's plugin as nonexistent.
//
// The id is taken from the listing; the host is not. The download URL is built
// from the configured BaseURL rather than the response's own `url` field, so
// whatever served the listing cannot nominate somewhere else for an executable
// to come from.
//
// This downloads bytes. It does not verify them and does not write them
// anywhere: the caller verifies against Checksums before anything is opened.
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
// An asset the release does not publish is a *NotFoundError naming the asset, so
// "there is no build for your machine" stays distinguishable from "the release
// could not be read"; the refusals from c.fetch travel out unchanged for the
// same reason.
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

// fetch performs one request and returns at most limit bytes, refusing rather
// than truncating when there are more: a truncated download would fail its
// checksum and be reported as a tampered archive.
//
// It reuses classify rather than being a second HTTP client, so that a rate
// limit, a forbidden and a genuine 404 stay three different errors on the
// download path exactly as they are on the search path.
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
