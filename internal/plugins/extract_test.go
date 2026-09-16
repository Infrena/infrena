package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry is one file in a tarball a test builds. Tests build their own
// archives rather than checking one in, because the interesting archives here
// are the malicious ones and a repository is a bad place to keep those.
type tarEntry struct {
	name string
	body string
	mode int64
}

// buildTar builds a gzipped tarball. Releases ship .tar.gz - see
// .github/workflows/release.yml, which produces infrena_0.7.1_linux_amd64.tar.gz
// - so this is the shape the extractor actually meets.
func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.name,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing header %q: %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("writing body %q: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildTarWithLink builds a tarball whose only entry is a symlink. The name is
// the one the manifest promises, so an extractor that checked only the name
// would follow it.
func buildTarWithLink(t *testing.T, name, target string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	hdr := &tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     name,
		Linkname: target,
		Mode:     0o777,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractsExactlyTheNamedBinary(t *testing.T) {
	tarball := buildTar(t, []tarEntry{
		{name: "README.md", body: "docs"},
		{name: "infrena-plugin-aws", body: "ELF", mode: 0o755},
		{name: "LICENSE", body: "text"},
	})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws")

	if err := ExtractBinary(tarball, "infrena-plugin-aws", dest, 1<<20); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "ELF" {
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

// The release workflow packs the binary INSIDE a versioned directory - see
// .github/workflows/release.yml, which tars dist/infrena_<version>_<os>_<arch>/
// whole - so the entry an archive really carries is nested, not top level.
// An extractor that matched the full path would work against every test above
// and against no actual release.
func TestExtractsABinaryNestedInsideTheReleaseDirectory(t *testing.T) {
	tarball := buildTar(t, []tarEntry{
		{name: "infrena-plugin-aws_0.4.0_linux_amd64/README.md", body: "docs"},
		{name: "infrena-plugin-aws_0.4.0_linux_amd64/infrena-plugin-aws", body: "ELF", mode: 0o755},
	})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws")

	if err := ExtractBinary(tarball, "infrena-plugin-aws", dest, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "ELF" {
		t.Fatalf("nested binary not extracted: %q %v", got, err)
	}
}

// THE ESCAPE. An entry whose path leaves the destination fails the whole
// archive - not sanitised, not skipped. An extractor that quietly drops this
// will happily extract whatever came next.
func TestATraversingEntryFailsTheWholeArchive(t *testing.T) {
	for _, name := range []string{
		"../../../.ssh/authorized_keys",
		"/etc/cron.d/evil",
		"sub/../../escape",
	} {
		t.Run(name, func(t *testing.T) {
			tarball := buildTar(t, []tarEntry{
				{name: name, body: "pwned"},
				{name: "infrena-plugin-aws", body: "ELF", mode: 0o755},
			})
			dir := t.TempDir()

			err := ExtractBinary(tarball, "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
			if err == nil {
				t.Fatal("an escaping entry was accepted")
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name the entry: %v", err)
			}
			// And nothing was written, including the legitimate binary.
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("wrote %d files despite refusing", len(entries))
			}
		})
	}
}

// The same escape AFTER the binary. Refusing the whole archive means refusing
// it whatever the order, and an extractor that stopped walking once it had
// what it wanted would write the binary out of an archive it never finished
// checking.
func TestATraversingEntryAfterTheBinaryStillFailsTheWholeArchive(t *testing.T) {
	tarball := buildTar(t, []tarEntry{
		{name: "infrena-plugin-aws", body: "ELF", mode: 0o755},
		{name: "../../../.ssh/authorized_keys", body: "pwned"},
	})
	dir := t.TempDir()

	err := ExtractBinary(tarball, "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
	if err == nil {
		t.Fatal("an escaping entry after the binary was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files despite refusing", len(entries))
	}
}

// A symlink is the same escape wearing different clothes.
func TestASymlinkEntryIsRefused(t *testing.T) {
	tarball := buildTarWithLink(t, "infrena-plugin-aws", "/etc/passwd")
	dir := t.TempDir()

	if err := ExtractBinary(tarball, "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20); err == nil {
		t.Fatal("a symlink was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files despite refusing", len(entries))
	}
}

// A hard link is the third spelling of the same idea, and archive/tar has a
// separate type for it.
func TestAHardLinkEntryIsRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeLink,
		Name:     "infrena-plugin-aws",
		Linkname: "/etc/passwd",
		Mode:     0o755,
		Format:   tar.FormatPAX,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()
	dir := t.TempDir()

	if err := ExtractBinary(buf.Bytes(), "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20); err == nil {
		t.Fatal("a hard link was accepted")
	}
}

// A few hundred kilobytes can expand to gigabytes. An install that fills the
// disk is a denial of service that survives a reboot.
func TestExtractionStopsAtTheByteCap(t *testing.T) {
	tarball := buildTar(t, []tarEntry{
		{name: "infrena-plugin-aws", body: strings.Repeat("A", 10<<20), mode: 0o755},
	})
	dir := t.TempDir()

	err := ExtractBinary(tarball, "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
	if err == nil {
		t.Fatal("an oversized entry was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %d files behind after refusing", len(entries))
	}
}

// A binary exactly at the cap is not oversized. An off-by-one here would
// refuse a legitimate release the day it crossed a round number.
func TestABinaryExactlyAtTheCapIsAccepted(t *testing.T) {
	body := strings.Repeat("A", 1024)
	tarball := buildTar(t, []tarEntry{{name: "infrena-plugin-aws", body: body, mode: 0o755}})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws")

	if err := ExtractBinary(tarball, "infrena-plugin-aws", dest, 1024); err != nil {
		t.Fatalf("a binary exactly at the cap was refused: %v", err)
	}
	if got, _ := os.ReadFile(dest); len(got) != len(body) {
		t.Errorf("extracted %d bytes, want %d", len(got), len(body))
	}
}

// The archive not containing what the manifest promised is an error naming
// both, not a silent success with nothing written.
func TestAnArchiveWithoutTheNamedBinaryIsAnError(t *testing.T) {
	tarball := buildTar(t, []tarEntry{{name: "something-else", body: "x", mode: 0o755}})
	dir := t.TempDir()

	err := ExtractBinary(tarball, "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
	if err == nil {
		t.Fatal("an archive missing the binary was accepted")
	}
	if !strings.Contains(err.Error(), "infrena-plugin-aws") {
		t.Errorf("error does not name what was expected: %v", err)
	}
}

// Two entries answering the name is not a choice to make. Extracting the first
// and ignoring the second would let an archive hide which file it actually
// installed.
func TestTwoEntriesWithTheBinaryNameAreRefused(t *testing.T) {
	tarball := buildTar(t, []tarEntry{
		{name: "a/infrena-plugin-aws", body: "ELF", mode: 0o755},
		{name: "b/infrena-plugin-aws", body: "OTHER", mode: 0o755},
	})
	dir := t.TempDir()

	err := ExtractBinary(tarball, "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
	if err == nil {
		t.Fatal("an archive carrying the name twice was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files despite refusing", len(entries))
	}
}

// Something that is not a gzipped tarball is refused, not read as one.
func TestSomethingThatIsNotAnArchiveIsRefused(t *testing.T) {
	dir := t.TempDir()

	if err := ExtractBinary([]byte("this is not a tarball"), "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20); err == nil {
		t.Fatal("a file that is not an archive was accepted")
	}
}

// buildTarWithDirectory builds the archive a release ACTUALLY ships: the
// versioned directory itself is an entry, because release.yml runs
// `tar -czf "$dir.tar.gz" -C dist "$(basename "$dir")"` and tar writes an entry
// for the directory it was handed.
func buildTarWithDirectory(t *testing.T, dir string, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     dir + "/",
		Mode:     0o755,
		Format:   tar.FormatPAX,
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     dir + "/" + e.name,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Format:   tar.FormatPAX,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// THE ARCHIVE A REAL RELEASE SHIPS. Every test above builds a tarball out of
// regular files only, which no `tar -czf` of a directory has ever produced: the
// directory is an entry too. An extractor that refused anything that was not a
// regular file passed all of them and would have refused every genuine install.
func TestTheDirectoryEntryEveryRealArchiveCarriesIsSkipped(t *testing.T) {
	tarball := buildTarWithDirectory(t, "infrena-plugin-aws_0.4.0_linux_amd64", []tarEntry{
		{name: "README.md", body: "docs"},
		{name: "infrena-plugin-aws", body: "ELF", mode: 0o755},
	})
	dest := filepath.Join(t.TempDir(), "infrena-plugin-aws")

	if err := ExtractBinary(tarball, "infrena-plugin-aws", dest, 1<<20); err != nil {
		t.Fatalf("the archive a real release ships was refused: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "ELF" {
		t.Fatalf("binary not extracted: %q %v", got, err)
	}
}

// Skipping directories must not let one stand in for the binary. A directory
// whose base name is the one being installed is still not a file, and the
// archive carries no binary at all.
func TestADirectoryNamedLikeTheBinaryIsNotInstalled(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "pkg/infrena-plugin-aws/",
		Mode:     0o755,
		Format:   tar.FormatPAX,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()
	dir := t.TempDir()

	err := ExtractBinary(buf.Bytes(), "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20)
	if err == nil {
		t.Fatal("a directory was installed as the binary")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files despite refusing", len(entries))
	}
}

// A traversing DIRECTORY entry is still the escape. Skipping directories is
// about not writing them, not about not looking at their names.
func TestATraversingDirectoryEntryStillFailsTheWholeArchive(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "../../../.ssh/",
		Mode:     0o755,
		Format:   tar.FormatPAX,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()
	dir := t.TempDir()

	if err := ExtractBinary(buf.Bytes(), "infrena-plugin-aws", filepath.Join(dir, "infrena-plugin-aws"), 1<<20); err == nil {
		t.Fatal("an escaping directory entry was accepted")
	}
}
