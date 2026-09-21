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

// ExtractBinary takes one file out of a release archive and refuses everything
// else.
//
// This is a security boundary. The bytes have been proved to be the ones the
// publisher released, which says nothing about what is inside them. Every rule
// is a refusal rather than a sanitisation, and is the same rule for both
// formats:
//
//   - An entry whose path leaves dest fails the whole archive, naming the
//     entry. It is not cleaned up and extracted, and not skipped: an archive
//     carrying one cannot be trusted for anything else in it either.
//   - Only regular files. No symlinks, hard links, devices, FIFOs or sockets. A
//     link's target can be outside dest, or be replaced between the check and
//     the use, and zip encodes links in the Unix mode in its external
//     attributes just as tar does.
//   - Exactly one file is written, the one wantName names. Everything else is
//     ignored, including the release directory entry both formats carry, which
//     is skipped rather than refused for the reason in extractTarInto.
//   - The binary is copied through an io.LimitedReader, because a small archive
//     can expand to gigabytes. The cap counts bytes actually written, never the
//     size the archive declares (tar's header Size, zip's UncompressedSize64):
//     believing a declared size means trusting the thing being unpacked.
//   - The write goes to a temporary file in dest's directory and is renamed into
//     place only after the whole archive has been walked without a refusal, so a
//     refusal never leaves something runnable behind.
//
// wantName is matched against each entry's base name, because releases pack the
// binary inside a versioned directory. Two entries answering the name are
// refused rather than resolved, since picking one would let an archive disguise
// what it installed.
//
// The format is decided by the magic number, not by the asset's file name, which
// is a convention the publisher controls: a mislabelled asset dispatched on its
// name would be read by guards written for a different format.
//
// It takes bytes rather than an io.Reader because zip is read from a central
// directory at the end of the file; install already holds the archive in memory
// from hashing it. Taking bytes rather than a URL also keeps this package off
// the network.
func ExtractBinary(archive []byte, wantName string, dest string, maxBytes int64) error {
	format, ok := detectFormat(archive)
	if !ok {
		return fmt.Errorf("the plugin archive is neither a gzipped tarball nor a zip file.\n\nReleases ship .tar.gz everywhere except windows, which ships .zip, and this is neither.\n\nSuggested action:\n  Check that the release asset for %s is one infrena can unpack. Nothing has been installed.", wantName)
	}

	// Created before either reader runs and removed on every path but the
	// successful rename below, so the rename is the only way a file appears at
	// dest.
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

// archiveFormat is one of the two shapes a release is published in.
type archiveFormat int

const (
	formatTarGz archiveFormat = iota
	formatZip
)

// detectFormat reads the magic number at the front of the archive.
//
// gzip is 1f 8b. zip is "PK" plus a two byte record signature: 03 04 is a local
// file header, which every archive with content starts with. 05 06 (an empty
// archive) and 07 08 (a spanned one) are accepted here so that they fail further
// down with something true about their contents rather than "this is not an
// archive".
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
// It reports whether the binary was found, and does not stop walking once it
// has been: an archive is refused whole, so an escaping entry after the binary
// still has to be seen. Returning early would write a binary out of an archive
// that was never finished checking.
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
		// A directory entry is skipped, not refused: releases are packed by
		// archiving a versioned directory, so refusing one would refuse every
		// real install. Nothing is given up — this code never creates a
		// directory, the entry is dropped before the name match so a directory
		// cannot stand in for the binary, and the traversal check above has
		// already run on it. A link stays a refusal because it writes
		// something whose target is not in the archive at all.
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return false, refuseEntryKind(hdr.Name, tarEntryKind(hdr.Typeflag), wantName)
		}

		if path.Base(path.Clean(hdr.Name)) != wantName {
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
// Every guard is the one extractTarInto applies, for the same reasons: a zip
// entry's name is attacker-controlled text exactly as a tar name is, and zip
// encodes a symlink in the Unix mode in its external attributes, so "only
// regular files" is enforced here too rather than assumed from the format.
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
		// Skipped for the reason set out in extractTarInto.
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

		// f.UncompressedSize64 is never consulted: a zip bomb declares whatever
		// it likes, and the cap below counts what actually comes out.
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
// Three shapes escape: an absolute path, a leading .., and a path such as
// sub/../../escape that only climbs out once cleaned — which is why the check is
// on the cleaned form rather than a prefix of the original. A Windows drive
// letter or backslash is refused too: tar and zip names are slash-separated by
// their formats. The error quotes the raw name, since that is what the archive
// carries and what its publisher has to be asked about.
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
// which wins, so this is a refusal rather than a preference for the first.
func refuseDuplicate(wantName, name string) error {
	return fmt.Errorf("the plugin archive contains %s more than once, at %q and elsewhere.\n\nWhich one would be installed is not a choice infrena makes on its own.\n\nSuggested action:\n  Report this to the publisher. Nothing from this archive has been installed.",
		wantName, name)
}

// tarEntryKind names a tar type flag in words, so a refusal says what was found
// rather than printing a byte.
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
