package plugins

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ExtractBinary takes ONE file out of a release archive and refuses everything
// else.
//
// THIS IS A SECURITY BOUNDARY, not a convenience wrapper around archive/tar.
// The bytes are what a publisher put on the internet, and by the time they
// reach here they have been proved to be the bytes that publisher released -
// which says nothing whatever about what is inside them. Every rule below is a
// REFUSAL rather than a sanitisation, and it is the SAME rule for both formats:
//
//   - An entry whose path leaves dest fails the WHOLE archive, naming the
//     entry. It is not cleaned up and quietly extracted, and it is not skipped:
//     an archive that contains one is an archive nothing in it can be trusted
//     from, including the legitimate binary that may follow.
//   - Only regular files. No symlinks, hard links, devices, FIFOs or sockets.
//     A symlink is the same escape wearing different clothes - the file it
//     points at can be outside dest, or can be replaced between the check and
//     the use - and zip encodes symlinks just as tar does, in the Unix mode it
//     carries in its external attributes.
//   - Exactly one file is written: the one wantName names. Everything else in
//     the archive is ignored, including the directory entry both formats
//     carry for the release folder, which is skipped rather than refused for
//     the reason set out in extractTarInto.
//   - The binary is copied through an io.LimitedReader. A few hundred kilobytes
//     expands to gigabytes, and an install that fills the disk is a denial of
//     service that survives a reboot. The cap is counted on the bytes ACTUALLY
//     WRITTEN, never on the size the archive declares - neither tar's header
//     Size nor zip's UncompressedSize64. A declared size is a number the
//     archive chooses, so believing it means deciding whether to unpack
//     something on the word of the thing being unpacked. Counting what comes
//     out needs no trust at all, and it is one count for a stored entry, a
//     deflated one and a gzipped tar member alike.
//   - The write goes to a temporary file in dest's directory and is renamed
//     into place only after the whole archive has been walked without a
//     refusal, so a refusal never leaves something runnable behind.
//
// wantName is matched against each entry's BASE name, because releases pack the
// binary inside a versioned directory: .github/workflows/release.yml archives
// dist/infrena_<version>_<os>_<arch>/ whole, so the real entry is
// infrena-plugin-aws_0.4.0_linux_amd64/infrena-plugin-aws rather than a
// top-level file. Two entries answering the name are refused rather than
// resolved, since picking one would let an archive disguise what it installed.
//
// THE FORMAT IS DECIDED BY THE BYTES, NOT BY THE FILE NAME. The asset name says
// .tar.gz or .zip, and the caller knows which it asked for, but the name is a
// convention the publisher controls while the bytes are what is about to be
// unpacked. Dispatching on the name would mean a mislabelled asset is handed to
// the wrong reader, which either fails with something untrue about the archive
// or, worse, gets read as a format whose guards were written for a different
// one. Two magic numbers settle it with no ambiguity: 1f 8b for gzip, PK for
// zip.
//
// It takes the bytes rather than an io.Reader because zip.NewReader needs an
// io.ReaderAt and a length - a zip is read from its central directory at the
// END of the file, so it cannot be streamed - and because install already holds
// the whole archive in memory, having just hashed it to check the published
// checksum. Reading it a second time would prove nothing about the copy being
// unpacked.
//
// OFFLINE. It takes bytes, never a URL, which is what keeps this package off
// the network and what makes every malicious archive above testable.
func ExtractBinary(archive []byte, wantName string, dest string, maxBytes int64) error {
	format, ok := detectFormat(archive)
	if !ok {
		return fmt.Errorf("the plugin archive is neither a gzipped tarball nor a zip file.\n\nReleases ship .tar.gz everywhere except windows, which ships .zip, and this is neither.\n\nSuggested action:\n  Check that the release asset for %s is one infrena can unpack. Nothing has been installed.", wantName)
	}

	// The temporary file is created before either reader runs and removed on
	// every path but the successful rename below, so the rename is the ONLY way
	// a file appears at dest - for both formats, from one place.
	destDir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(destDir, "."+filepath.Base(dest)+"-*.part")
	if err != nil {
		return fmt.Errorf("creating a temporary file for %s in %s: %w", wantName, destDir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	var found bool
	switch format {
	case formatTarGz:
		found, err = extractTarInto(tmp, archive, wantName, maxBytes)
	case formatZip:
		found, err = extractZipInto(tmp, archive, wantName, maxBytes)
	}
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("the plugin archive does not contain %s.\n\nThe manifest names %s as the binary this plugin ships, and no entry in the archive has that name.\n\nSuggested action:\n  Report this to the publisher. Nothing has been installed.",
			wantName, wantName)
	}

	// 0755: this is about to be executed, and it is the user's own plugin
	// directory. Set before the rename so the file is never briefly present at
	// dest with the wrong mode.
	if err := tmp.Chmod(0o755); err != nil {
		return fmt.Errorf("making %s executable: %w", wantName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", wantName, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("moving %s into %s: %w", wantName, destDir, err)
	}
	return nil
}

// archiveFormat is one of the two shapes .github/workflows/release.yml
// produces.
type archiveFormat int

const (
	formatTarGz archiveFormat = iota
	formatZip
)

// detectFormat reads the magic number at the front of the archive.
//
// gzip is 1f 8b. zip is "PK" followed by a two byte record signature: 03 04 is
// a local file header, which every archive with content in it starts with,
// while 05 06 (an empty archive: nothing but an end-of-central-directory
// record) and 07 08 (a spanned archive) are accepted here so that they fail
// further down with something true about their contents rather than with "this
// is not an archive".
func detectFormat(archive []byte) (archiveFormat, bool) {
	if len(archive) >= 2 && archive[0] == 0x1f && archive[1] == 0x8b {
		return formatTarGz, true
	}
	if len(archive) >= 4 && archive[0] == 'P' && archive[1] == 'K' {
		switch {
		case archive[2] == 0x03 && archive[3] == 0x04,
			archive[2] == 0x05 && archive[3] == 0x06,
			archive[2] == 0x07 && archive[3] == 0x08:
			return formatZip, true
		}
	}
	return 0, false
}

// extractTarInto walks a gzipped tarball, writing only wantName into dst.
//
// It reports whether the binary was found. It does NOT stop walking once it has
// been: an archive is refused whole, so an escaping entry AFTER the binary has
// to be seen. An extractor that returned early would write a binary out of an
// archive it never finished checking.
func extractTarInto(dst io.Writer, archive []byte, wantName string, maxBytes int64) (bool, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return false, fmt.Errorf("the plugin archive is not a readable gzipped tarball: %w.\n\nSuggested action:\n  Check that the release asset for %s is the .tar.gz infrena expects.", err, wantName)
	}
	defer zr.Close()

	found := false
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("reading the plugin archive for %s: %w", wantName, err)
		}

		if err := checkEntryPath(hdr.Name); err != nil {
			return false, err
		}
		// A DIRECTORY ENTRY IS SKIPPED, NOT REFUSED, and unlike every other
		// rule on this page that is not a softening. release.yml runs
		// `tar -czf` and `zip -qr` over dist/<name>_<version>_<os>_<arch>/,
		// and both tools write an entry for the directory itself, so EVERY
		// genuine release archive contains one. Refusing it would refuse
		// every real install while passing every test that built its own
		// archive out of regular files.
		//
		// Nothing is given up by skipping it. This code never creates a
		// directory: dest already exists, exactly one file is written into
		// it, and the entry is dropped BEFORE the name match so a directory
		// called infrena-plugin-aws/ cannot stand in for the binary. The
		// traversal check above has already run on it, so a directory entry
		// that climbs out still fails the whole archive. A link, by contrast,
		// is a request to write something whose target is not in the archive
		// at all, which is why that stays a refusal.
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return false, refuseEntryKind(hdr.Name, tarEntryKind(hdr.Typeflag), wantName)
		}

		if path.Base(path.Clean(hdr.Name)) != wantName {
			// Ignored, not written. The archive is free to ship a README and
			// a licence; infrena simply takes nothing from them.
			continue
		}
		if found {
			return false, refuseDuplicate(wantName, hdr.Name)
		}
		found = true

		if err := copyCapped(dst, tr, wantName, maxBytes); err != nil {
			return false, err
		}
	}
	return found, nil
}

// extractZipInto walks a zip, writing only wantName into dst.
//
// Every guard is the one extractTarInto applies, and for the same reasons. Zip
// has its own long history of traversal bugs - the name in a zip entry is
// attacker-controlled text exactly as a tar name is - and it encodes a symlink
// in the Unix mode it stores in its external attributes, so "only regular
// files" has to be enforced here too rather than assumed from the format.
//
// The one thing zip does differently is that it is read from the central
// directory at the end of the file, so there is no streaming reader: zip.
// NewReader takes an io.ReaderAt and a size. That is why ExtractBinary holds
// the bytes.
func extractZipInto(dst io.Writer, archive []byte, wantName string, maxBytes int64) (bool, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return false, fmt.Errorf("the plugin archive is not a readable zip file: %w.\n\nSuggested action:\n  Check that the release asset for %s is the .zip infrena expects. Nothing has been installed.", err, wantName)
	}

	found := false
	for _, f := range zr.File {
		if err := checkEntryPath(f.Name); err != nil {
			return false, err
		}
		mode := f.Mode()
		// Skipped for the reason set out in extractTarInto: `zip -qr` writes
		// an entry for the release directory, so every genuine archive has
		// one, and nothing here ever creates a directory anyway.
		if mode.IsDir() {
			continue
		}
		if !mode.IsRegular() {
			return false, refuseEntryKind(f.Name, fileModeKind(mode), wantName)
		}

		if path.Base(path.Clean(f.Name)) != wantName {
			continue
		}
		if found {
			return false, refuseDuplicate(wantName, f.Name)
		}
		found = true

		// f.UncompressedSize64 is never consulted. A zip bomb declares
		// whatever it likes, and the cap below counts what comes out instead.
		// archive/zip happens to stop a reader at the declared size too, so an
		// entry that UNDERSTATES itself is refused by the standard library
		// before this cap sees it; that is a second line, not this one.
		rc, err := f.Open()
		if err != nil {
			return false, fmt.Errorf("reading %s out of the plugin archive: %w", wantName, err)
		}
		err = copyCapped(dst, rc, wantName, maxBytes)
		rc.Close()
		if err != nil {
			return false, err
		}
	}
	return found, nil
}

// copyCapped writes one entry's decompressed bytes, refusing when there are
// more than maxBytes of them.
//
// maxBytes+1 so that reading exactly maxBytes is not mistaken for the
// truncation of something larger: a binary the same size as the cap is not over
// it.
func copyCapped(dst io.Writer, src io.Reader, wantName string, maxBytes int64) error {
	written, err := io.Copy(dst, io.LimitReader(src, maxBytes+1))
	if err != nil {
		return fmt.Errorf("reading %s out of the plugin archive: %w", wantName, err)
	}
	if written > maxBytes {
		return fmt.Errorf("%s in the plugin archive is larger than the %d byte limit infrena will extract.\n\nA small archive can expand to an enormous file, so extraction stops at the limit rather than filling the disk.\n\nSuggested action:\n  Check that this is the release you meant to install. Nothing has been installed.",
			wantName, maxBytes)
	}
	return nil
}

// checkEntryPath refuses a name that does not stay inside the directory being
// extracted into.
//
// The RAW name is what the error quotes, because the raw name is what the
// archive carries and what its publisher has to be asked about. Cleaning it
// first and reporting the result would describe a path nobody can find in the
// file.
//
// Three shapes, one rule. An absolute path writes wherever it likes. A leading
// .. climbs out. And a path that is neither, such as sub/../../escape, climbs
// out once cleaned - which is exactly why the check is on the cleaned form
// rather than on a prefix of the original. A Windows drive letter or a
// backslash separator is refused too: both tar and zip names are
// slash-separated by their formats, so anything carrying a backslash is not a
// path either archive should contain.
func checkEntryPath(name string) error {
	cleaned := path.Clean(name)
	bad := path.IsAbs(name) ||
		strings.HasPrefix(name, "/") ||
		strings.Contains(name, `\`) ||
		cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") ||
		filepath.IsAbs(cleaned) ||
		filepath.VolumeName(name) != ""
	if !bad {
		return nil
	}
	return fmt.Errorf("the plugin archive contains %q, which would be written outside the directory being installed into.\n\nThe whole archive is refused rather than that entry being tidied up: an archive carrying one path like this is not one anything else in it can be trusted from.\n\nSuggested action:\n  Report this to the publisher. Nothing has been installed.", name)
}

// refuseEntryKind is the refusal for an entry that is not a regular file.
func refuseEntryKind(name, kind, wantName string) error {
	return fmt.Errorf("the plugin archive contains %q, which is %s rather than a regular file.\n\nAn archive that installs links or devices is refused whole: a link can point outside the directory being installed into, or be replaced between being checked and being used.\n\nSuggested action:\n  Report this to the publisher of %s. Nothing from this archive has been installed.",
		name, kind, wantName)
}

// refuseDuplicate is the refusal for an archive carrying the binary's name
// twice. Zip allows duplicate names outright and two readers can disagree about
// which one wins, which is the whole reason this is a refusal rather than a
// preference for the first.
func refuseDuplicate(wantName, name string) error {
	return fmt.Errorf("the plugin archive contains %s more than once, at %q and elsewhere.\n\nWhich one would be installed is not a choice infrena makes on its own.\n\nSuggested action:\n  Report this to the publisher. Nothing from this archive has been installed.",
		wantName, name)
}

// tarEntryKind names a tar type flag in words, so the refusal says what was
// found rather than printing a byte.
func tarEntryKind(flag byte) string {
	switch flag {
	case tar.TypeSymlink:
		return "a symbolic link"
	case tar.TypeLink:
		return "a hard link"
	case tar.TypeDir:
		return "a directory"
	case tar.TypeChar:
		return "a character device"
	case tar.TypeBlock:
		return "a block device"
	case tar.TypeFifo:
		return "a named pipe"
	default:
		return "an entry of type " + string(flag)
	}
}

// fileModeKind names what a zip entry's Unix mode says it is, for the same
// reason.
func fileModeKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symbolic link"
	case mode.IsDir():
		return "a directory"
	case mode&fs.ModeCharDevice != 0:
		return "a character device"
	case mode&fs.ModeDevice != 0:
		return "a block device"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	default:
		return "an entry with mode " + mode.String()
	}
}
