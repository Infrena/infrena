package plugins

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE WINDOWS HALF OF EVERY TEST IN extract_test.go.
//
// release.yml zips the windows builds and tars everything else, so a table that
// only covered tarballs covered six of the eight platforms a release ships and
// left install broken on the other two. Zip is not a thinner threat model than
// tar either: it has its own long history of traversal bugs, it stores a Unix
// mode so it can encode a symlink, it permits two entries with the same name,
// and its headers declare an uncompressed size that a bomb simply lies about.
// So this is the same table, entry for entry, built against archive/zip.

// zipEntry is one file in a zip a test builds. Tests build their own archives
// rather than checking one in, for the same reason the tar tests do: the
// interesting archives are the malicious ones.
type zipEntry struct {
	name string
	body string
	mode fs.FileMode
}

// buildZip builds a zip of regular files. Deflate rather than Store, because
// that is what `zip -qr` produces and because a stored entry could not express
// the bomb below.
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("creating %q: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("writing %q: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildZipWithMode builds a zip whose only entry carries an arbitrary Unix
// mode, which is how zip encodes a symlink, a device or a FIFO: in the upper
// bits of the external attributes, decoded by zip.FileHeader.Mode.
func buildZipWithMode(t *testing.T, name string, mode fs.FileMode, body string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(mode)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildZipWithDirectory builds the archive a real windows release ships: the
// versioned directory is an entry of its own, because release.yml runs
// `zip -qr "$(basename "$dir").zip" "$(basename "$dir")"` and zip writes one.
func buildZipWithDirectory(t *testing.T, dir string, entries []zipEntry) []byte {
	t.Helper()

	all := []zipEntry{{name: dir + "/", mode: fs.ModeDir | 0o755}}
	for _, e := range entries {
		e.name = dir + "/" + e.name
		all = append(all, e)
	}
	return buildZip(t, all)
}

func TestZipExtractsExactlyTheNamedBinary(t *testing.T) {
	archive := buildZip(t, []zipEntry{
		{name: "README.md", body: "docs"},
		{name: "infrena-plugin-aws.exe", body: "MZ", mode: 0o755},
		{name: "LICENSE", body: "text"},
	})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws.exe")

	if err := ExtractBinary(archive, "infrena-plugin-aws.exe", dest, 1<<20); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "MZ" {
		t.Fatalf("binary not extracted: %q %v", got, err)
	}
	// Everything else is ignored, not written.
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 1 {
		t.Errorf("extracted %d files, want only the binary", len(entries))
	}
	fi, _ := os.Stat(dest)
	if fi.Mode().Perm()&0o111 == 0 {
		t.Error("extracted binary is not executable")
	}
}

// The release workflow packs the binary INSIDE a versioned directory here too,
// so the entry a real windows archive carries is nested rather than top level.
func TestZipExtractsABinaryNestedInsideTheReleaseDirectory(t *testing.T) {
	archive := buildZipWithDirectory(t, "infrena-plugin-aws_0.4.0_windows_amd64", []zipEntry{
		{name: "README.md", body: "docs"},
		{name: "infrena-plugin-aws.exe", body: "MZ", mode: 0o755},
	})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws.exe")

	if err := ExtractBinary(archive, "infrena-plugin-aws.exe", dest, 1<<20); err != nil {
		t.Fatalf("the archive a real windows release ships was refused: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "MZ" {
		t.Fatalf("nested binary not extracted: %q %v", got, err)
	}
}

// THE ESCAPE, in zip. Zip Slip is the same bug as tar traversal with a
// different CVE number, and a zip name is attacker-controlled text exactly as a
// tar name is. A backslash is included because a zip written on windows can
// carry one even though the format says the separator is a slash.
func TestZipATraversingEntryFailsTheWholeArchive(t *testing.T) {
	for _, name := range []string{
		"../../../.ssh/authorized_keys",
		"/etc/cron.d/evil",
		"sub/../../escape",
		`..\..\windows\system32\evil.dll`,
	} {
		t.Run(name, func(t *testing.T) {
			archive := buildZip(t, []zipEntry{
				{name: name, body: "pwned"},
				{name: "infrena-plugin-aws.exe", body: "MZ", mode: 0o755},
			})
			dir := t.TempDir()

			err := ExtractBinary(archive, "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20)
			if err == nil {
				t.Fatal("an escaping entry was accepted")
			}
			// %q rather than the raw name: the error quotes the entry, and
			// the backslash case comes back escaped.
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", name)) {
				t.Errorf("error does not name the entry: %v", err)
			}
			// And nothing was written, including the legitimate binary.
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("wrote %d files despite refusing", len(entries))
			}
		})
	}
}

// The same escape AFTER the binary. This matters more in zip than in tar: the
// entries come from the central directory, so an extractor is even more tempted
// to stop as soon as it has found what it wants.
func TestZipATraversingEntryAfterTheBinaryStillFailsTheWholeArchive(t *testing.T) {
	archive := buildZip(t, []zipEntry{
		{name: "infrena-plugin-aws.exe", body: "MZ", mode: 0o755},
		{name: "../../../.ssh/authorized_keys", body: "pwned"},
	})
	dir := t.TempDir()

	err := ExtractBinary(archive, "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20)
	if err == nil {
		t.Fatal("an escaping entry after the binary was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files despite refusing", len(entries))
	}
}

// A symlink is the same escape wearing different clothes, and zip can wear them
// too: the mode lives in the external attributes and the body is the target
// path. An extractor that assumed zip has no symlinks would write /etc/passwd's
// name into a file and call it a binary.
func TestZipASymlinkEntryIsRefused(t *testing.T) {
	archive := buildZipWithMode(t, "infrena-plugin-aws.exe", fs.ModeSymlink|0o777, "/etc/passwd")
	dir := t.TempDir()

	if err := ExtractBinary(archive, "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20); err == nil {
		t.Fatal("a symlink was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files despite refusing", len(entries))
	}
}

// The rest of the non-regular kinds, which zip encodes the same way.
func TestZipADeviceOrPipeEntryIsRefused(t *testing.T) {
	for name, mode := range map[string]fs.FileMode{
		"device":     fs.ModeDevice | 0o666,
		"named pipe": fs.ModeNamedPipe | 0o666,
		"socket":     fs.ModeSocket | 0o666,
	} {
		t.Run(name, func(t *testing.T) {
			archive := buildZipWithMode(t, "infrena-plugin-aws.exe", mode, "x")
			dir := t.TempDir()

			if err := ExtractBinary(archive, "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20); err == nil {
				t.Fatalf("a %s was accepted", name)
			}
		})
	}
}

// THE ZIP BOMB, which is the classic case rather than a hypothetical one: ten
// megabytes of one repeated byte deflates to a few kilobytes, so nothing about
// the archive's size warns anybody.
func TestZipExtractionStopsAtTheByteCap(t *testing.T) {
	archive := buildZip(t, []zipEntry{
		{name: "infrena-plugin-aws.exe", body: strings.Repeat("A", 10<<20), mode: 0o755},
	})
	if len(archive) > 1<<20 {
		t.Fatalf("the bomb compressed to %d bytes, which is not small enough to make the point", len(archive))
	}
	dir := t.TempDir()

	err := ExtractBinary(archive, "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20)
	if err == nil {
		t.Fatal("an oversized entry was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %d files behind after refusing", len(entries))
	}
}

// THE HEADER LIES AND IS NOT BELIEVED. A zip entry declares its uncompressed
// size, and a hostile one declares whatever gets it past a check: this archive
// says ten bytes and carries ten megabytes.
//
// Nothing oversized reaches disk, and it is worth being exact about why, since
// two different guards are in play. TestZipExtractionStopsAtTheByteCap covers a
// bomb that declares its real, enormous size, which is the case this package's
// own cap on written bytes refuses. The lie below is caught one layer lower:
// archive/zip stops a reader at the size the entry declared, so the extra bytes
// never arrive and the entry fails as a malformed zip. Both are refusals with
// nothing written, which is what this pins.
func TestZipAnEntryThatUnderstatesItsSizeIsRefused(t *testing.T) {
	body := []byte(strings.Repeat("A", 10<<20))

	var deflated bytes.Buffer
	fw, err := flate.NewWriter(&deflated, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{
		Name:               "infrena-plugin-aws.exe",
		Method:             zip.Deflate,
		CRC32:              crc32.ChecksumIEEE(body),
		CompressedSize64:   uint64(deflated.Len()),
		UncompressedSize64: 10, // the lie
	}
	hdr.SetMode(0o755)
	w, err := zw.CreateRaw(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(deflated.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	err = ExtractBinary(buf.Bytes(), "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20)
	if err == nil {
		t.Fatal("an entry that understated its size was accepted")
	}
	if !strings.Contains(err.Error(), "infrena-plugin-aws.exe") {
		t.Errorf("refusal does not name the binary: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %d files behind after refusing", len(entries))
	}
}

// A binary exactly at the cap is not oversized. An off-by-one here would refuse
// a legitimate release the day it crossed a round number.
func TestZipABinaryExactlyAtTheCapIsAccepted(t *testing.T) {
	body := strings.Repeat("A", 1024)
	archive := buildZip(t, []zipEntry{{name: "infrena-plugin-aws.exe", body: body, mode: 0o755}})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws.exe")

	if err := ExtractBinary(archive, "infrena-plugin-aws.exe", dest, 1024); err != nil {
		t.Fatalf("a binary exactly at the cap was refused: %v", err)
	}
	if got, _ := os.ReadFile(dest); len(got) != len(body) {
		t.Errorf("extracted %d bytes, want %d", len(got), len(body))
	}
}

// The archive not containing what the manifest promised is an error naming it,
// not a silent success with nothing written.
func TestZipAnArchiveWithoutTheNamedBinaryIsAnError(t *testing.T) {
	archive := buildZip(t, []zipEntry{{name: "something-else.exe", body: "x", mode: 0o755}})
	dir := t.TempDir()

	err := ExtractBinary(archive, "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20)
	if err == nil {
		t.Fatal("an archive missing the binary was accepted")
	}
	if !strings.Contains(err.Error(), "infrena-plugin-aws.exe") {
		t.Errorf("error does not name what was expected: %v", err)
	}
}

// Two entries answering the name is not a choice to make, and zip makes this
// sharper than tar: duplicate names are permitted outright, and two readers can
// legitimately disagree about which one wins.
func TestZipTwoEntriesWithTheBinaryNameAreRefused(t *testing.T) {
	archive := buildZip(t, []zipEntry{
		{name: "a/infrena-plugin-aws.exe", body: "MZ", mode: 0o755},
		{name: "b/infrena-plugin-aws.exe", body: "OTHER", mode: 0o755},
	})
	dir := t.TempDir()

	err := ExtractBinary(archive, "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20)
	if err == nil {
		t.Fatal("an archive carrying the name twice was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files despite refusing", len(entries))
	}
}

// A directory entry is skipped, not refused, exactly as in tar, and still
// cannot stand in for the binary.
func TestZipADirectoryNamedLikeTheBinaryIsNotInstalled(t *testing.T) {
	archive := buildZip(t, []zipEntry{{name: "pkg/infrena-plugin-aws.exe/", mode: fs.ModeDir | 0o755}})
	dir := t.TempDir()

	err := ExtractBinary(archive, "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20)
	if err == nil {
		t.Fatal("a directory was installed as the binary")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files despite refusing", len(entries))
	}
}

// Something that is neither format is refused, not read as one.
func TestZipSomethingThatIsNotAnArchiveIsRefused(t *testing.T) {
	dir := t.TempDir()

	if err := ExtractBinary([]byte("PJ not a zip either"), "infrena-plugin-aws.exe", filepath.Join(dir, "infrena-plugin-aws.exe"), 1<<20); err == nil {
		t.Fatal("a file that is not an archive was accepted")
	}
}

// The format comes from the bytes. A zip is unpacked as a zip whatever anybody
// called it, and a truncated one is refused rather than being handed to the tar
// reader and reported as a broken tarball.
func TestTheFormatIsDecidedByTheMagicBytes(t *testing.T) {
	archive := buildZip(t, []zipEntry{{name: "infrena-plugin-aws", body: "ELF", mode: 0o755}})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws")

	// A zip asked for under the name a tarball would have. Nothing about the
	// call says which format this is.
	if err := ExtractBinary(archive, "infrena-plugin-aws", dest, 1<<20); err != nil {
		t.Fatalf("a zip was not recognised from its bytes: %v", err)
	}

	dir := t.TempDir()
	err := ExtractBinary(archive[:len(archive)/2], "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
	if err == nil {
		t.Fatal("a truncated zip was accepted")
	}
	if !strings.Contains(err.Error(), "zip") {
		t.Errorf("a truncated zip was not reported as a zip: %v", err)
	}
}
