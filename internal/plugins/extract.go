package plugins

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ExtractBinary takes ONE file out of a gzipped tarball and refuses everything
// else.
//
// THIS IS A SECURITY BOUNDARY, not a convenience wrapper around archive/tar.
// The reader is bytes a publisher put on the internet, and by the time they
// reach here they have been proved to be the bytes that publisher released -
// which says nothing whatever about what is inside them. Every rule below is a
// REFUSAL rather than a sanitisation:
//
//   - An entry whose path leaves dest fails the WHOLE archive, naming the
//     entry. It is not cleaned up and quietly extracted, and it is not skipped:
//     an archive that contains one is an archive nothing in it can be trusted
//     from, including the legitimate binary that may follow.
//   - Only tar.TypeReg. No symlinks, hard links, devices, FIFOs or directories
//     are created. A symlink is the same escape wearing different clothes - the
//     file it points at can be outside dest, or can be replaced between the
//     check and the use.
//   - Exactly one file is written: the one wantName names. Everything else in
//     the archive is ignored.
//   - The binary is copied through an io.LimitedReader. A few hundred kilobytes
//     of gzip expands to gigabytes, and an install that fills the disk is a
//     denial of service that survives a reboot. The header's Size field is not
//     trusted for this; the cap is enforced on the bytes actually read.
//   - The write goes to a temporary file in dest's directory and is renamed
//     into place only after the whole archive has been walked without a
//     refusal, so a refusal never leaves something runnable behind.
//
// wantName is matched against each entry's BASE name, because releases pack the
// binary inside a versioned directory: .github/workflows/release.yml tars
// dist/infrena_<version>_<os>_<arch>/ whole, so the real entry is
// infrena-plugin-aws_0.4.0_linux_amd64/infrena-plugin-aws rather than a
// top-level file. Two entries answering the name are refused rather than
// resolved, since picking one would let an archive disguise what it installed.
//
// OFFLINE. It takes an io.Reader, never a URL, which is what keeps this package
// off the network and what makes every malicious archive above testable.
func ExtractBinary(r io.Reader, wantName string, dest string, maxBytes int64) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the plugin archive is not a gzipped tarball: %w.\n\nSuggested action:\n  Check that the release asset for %s is the .tar.gz infrena expects.", err, wantName)
	}
	defer zr.Close()

	destDir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(destDir, "."+filepath.Base(dest)+"-*.part")
	if err != nil {
		return fmt.Errorf("creating a temporary file for %s in %s: %w", wantName, destDir, err)
	}
	tmpName := tmp.Name()
	// Removed on every path that is not the successful rename, which makes the
	// rename below the ONLY way a file appears at dest.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	found := false
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the plugin archive for %s: %w", wantName, err)
		}

		if err := checkEntryPath(hdr.Name); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("the plugin archive contains %q, which is %s rather than a regular file.\n\nAn archive that installs links or devices is refused whole: a link can point outside the directory being installed into, or be replaced between being checked and being used.\n\nSuggested action:\n  Report this to the publisher of %s. Nothing from this archive has been installed.",
				hdr.Name, entryKind(hdr.Typeflag), wantName)
		}

		if path.Base(path.Clean(hdr.Name)) != wantName {
			// Ignored, not written. The archive is free to ship a README and
			// a licence; infrena simply takes nothing from them.
			continue
		}
		if found {
			return fmt.Errorf("the plugin archive contains %s more than once, at %q and elsewhere.\n\nWhich one would be installed is not a choice infrena makes on its own.\n\nSuggested action:\n  Report this to the publisher. Nothing from this archive has been installed.",
				wantName, hdr.Name)
		}
		found = true

		// maxBytes+1 so that reading exactly maxBytes is not mistaken for the
		// truncation of something larger: a binary the same size as the cap is
		// not over it.
		written, err := io.Copy(tmp, io.LimitReader(tr, maxBytes+1))
		if err != nil {
			return fmt.Errorf("reading %s out of the plugin archive: %w", wantName, err)
		}
		if written > maxBytes {
			return fmt.Errorf("%s in the plugin archive is larger than the %d byte limit infrena will extract.\n\nA small archive can expand to an enormous file, so extraction stops at the limit rather than filling the disk.\n\nSuggested action:\n  Check that this is the release you meant to install. Nothing has been installed.",
				wantName, maxBytes)
		}
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
// backslash separator is refused too: tar names are slash-separated by the
// format, so anything carrying a backslash is not a path this archive should
// contain.
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

// entryKind names a tar type flag in words, so the refusal above says what was
// found rather than printing a byte.
func entryKind(flag byte) string {
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
