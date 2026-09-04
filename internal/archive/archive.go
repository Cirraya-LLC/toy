// Package archive unpacks archives safely. Iron rule 7: path traversal, absolute
// paths, and symlinks that escape the destination directory are all refused.
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrTraversal   = errors.New("archive: path escapes the destination directory")
	ErrUnsupported = errors.New("archive: unsupported archive format")
	ErrTooLarge    = errors.New("archive: extracted size exceeds the limit")
)

// MaxTotalBytes caps the total extracted size. This is the zip bomb defence.
const MaxTotalBytes = 2 << 30 // 2 GiB

// maxLinkTargetBytes caps how long a symlink target may be. In zip, a link target IS
// the entry's file content, so with no cap a "symlink" is just an arbitrarily large
// file smuggled in under another name.
const maxLinkTargetBytes = 4096

// maxEntries caps the member count. A byte limit does nothing against a package of a
// few million empty files: that bomb burns inodes, not bytes.
const maxEntries = 100000

type kind int

const (
	kindNone kind = iota
	kindTarGz
	kindTar
	kindZip
	kindTarXz
	kindTarBz2
)

// formatOf decides the format from the file name alone, never from the content.
// IsArchive shares this one function so "we recognise it" and "we can actually unpack
// it" can never drift apart.
func formatOf(name string) kind {
	n := strings.ToLower(filepath.Base(name))
	switch {
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return kindTarGz
	case strings.HasSuffix(n, ".tar.bz2"), strings.HasSuffix(n, ".tbz2"), strings.HasSuffix(n, ".tbz"):
		return kindTarBz2
	case strings.HasSuffix(n, ".tar.xz"):
		return kindTarXz
	case strings.HasSuffix(n, ".tar"):
		return kindTar
	case strings.HasSuffix(n, ".zip"):
		return kindZip
	}
	return kindNone
}

// IsArchive reports whether a name looks like an archive we know.
// Known: .tar.gz .tgz .tar.xz (recognised, not unpacked - Extract returns
// ErrUnsupported) .tar .zip
// Anything else, a bare binary for instance, is false: the caller should install it
// as a single file.
func IsArchive(name string) bool { return formatOf(name) != kindNone }

// Extract unpacks src into destDir, which must already exist.
// It guarantees:
//   - every member's final path, after filepath.Clean, still sits inside destDir,
//     otherwise ErrTraversal
//   - absolute member paths are refused
//   - symlinks: if the target resolves outside destDir, the whole archive is refused
//   - contentless irregular members (device nodes, FIFOs, sockets) are skipped; an
//     unknown type that declares content is refused
//   - passing MaxTotalBytes aborts the extraction and cleans up
//
// If the extracted tree turns out to be EXACTLY ONE top-level entry AND that entry is
// a directory, that layer is stripped (see stripTopDir). The relative paths returned
// are the stripped ones.
func Extract(src, destDir string) ([]string, error) {
	return extractTo(src, destDir, MaxTotalBytes)
}

// extractTo is the body of Extract with the byte limit as a parameter, so tests can
// exercise ErrTooLarge on a tiny budget instead of actually writing 2 GiB.
func extractTo(src, destDir string, limit int64) ([]string, error) {
	switch formatOf(src) {
	case kindNone:
		return nil, fmt.Errorf("%w: %s. Only .tar.gz/.tgz/.tar/.zip are supported; install a bare binary as a single file instead",
			ErrUnsupported, filepath.Base(src))
	case kindTarXz:
		return nil, fmt.Errorf("%w: %s needs an external xz to unpack. Ask the author to ship .tar.gz or .zip instead",
			ErrUnsupported, filepath.Base(src))
	}

	dest, err := filepath.Abs(destDir)
	if err != nil {
		return nil, fmt.Errorf("archive: cannot resolve %q to an absolute path: %w", destDir, err)
	}
	dest = filepath.Clean(dest)
	fi, err := os.Stat(dest)
	if err != nil {
		return nil, fmt.Errorf("archive: destination directory %s is unusable: %w", dest, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("archive: destination %s is not a directory", dest)
	}

	e := &extractor{dest: dest, limit: limit, regular: map[string]bool{}}
	if err := e.run(src); err != nil {
		// Any failure wipes whatever this run wrote: half a package is harder to
		// diagnose than no package, and on the ErrTooLarge path a pile of bytes is
		// already sitting on disk.
		e.cleanup()
		return nil, err
	}
	if err := e.stripTopDir(); err != nil {
		e.cleanup()
		return nil, err
	}
	return e.files, nil
}

// stripTopDir removes the single top-level directory, the equivalent of
// `tar --strip-components=1`.
//
// cargo-dist and half the goreleaser configs out there wrap everything in one
// `ripgrep-15.2.0-x86_64-unknown-linux-musl/` directory, and that name pins BOTH the
// version AND the target triple. The right-hand side of [bin] in toy.toml is a single
// string: it does not vary by platform and does not expand {version}, so it simply
// cannot express that name. Without stripping, the entire modern Rust CLI shelf
// (ripgrep / fd / bat / delta / eza) is unpackageable. eget, ubi and cargo-binstall
// all strip; this is what users expect.
//
// The test is EXACTLY ONE top-level entry AND it is a real directory:
//   - Two or more: no strip. Whichever one you picked, you would drop the other.
//   - The single entry is a file: no strip (that is the bare-binary shape).
//   - The single entry is a symlink: no strip either. DirEntry.IsDir() has lstat
//     semantics, so a symlink reads false, which means "follow a link and move the
//     contents outside destDir" is not a thing that can happen here.
//   - An empty directory: no strip. Stripping would leave destDir empty, and that is
//     not "one layer less", it is "nothing at all".
//
// The name only ever comes from os.ReadDir, which always hands back a single path
// element - never "." or "..", never containing a separator - so the Join below
// necessarily lands inside destDir. Iron rule 7 is enforced in resolve(); this step
// does not introduce a second source of names that would need sanitising.
func (e *extractor) stripTopDir() error {
	ents, err := os.ReadDir(e.dest)
	if err != nil {
		return fmt.Errorf("archive: cannot read %s before stripping the top-level directory: %w", e.dest, err)
	}
	if len(ents) != 1 || !ents[0].IsDir() {
		return nil
	}
	top := ents[0].Name()
	topDir := filepath.Join(e.dest, top)
	inner, err := os.ReadDir(topDir)
	if err != nil {
		return fmt.Errorf("archive: cannot read %s while stripping the top-level directory: %w", topDir, err)
	}
	if len(inner) == 0 {
		return nil
	}
	prefix := top + string(filepath.Separator)
	if !e.stripKeepsLinksInside(prefix) {
		return nil
	}
	// Move that directory aside before hoisting its contents, or `pkg/pkg` - an inner
	// entry named like the top level, which happens whenever the package name matches
	// the directory name - collides: renaming onto a non-empty directory is ENOTEMPTY,
	// onto an empty one is EISDIR, and either one makes a perfectly good package
	// impossible to install. Once it is moved aside, every target name is free.
	// The staging name shares top's parent, hence the same filesystem, so the rename
	// never crosses devices.
	staged := topDir + ".toy-strip"
	if err := os.Rename(topDir, staged); err != nil {
		return fmt.Errorf("archive: cannot move top-level directory %s aside while stripping it: %w", top, err)
	}
	for _, ent := range inner {
		if err := os.Rename(filepath.Join(staged, ent.Name()), filepath.Join(e.dest, ent.Name())); err != nil {
			return fmt.Errorf("archive: stripping top-level directory %s: cannot move %s up: %w", top, ent.Name(), err)
		}
	}
	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("archive: cannot remove top-level directory %s after stripping it: %w", top, err)
	}
	// The relative paths we hand back have to be stripped too, or they stop matching
	// the layout on disk - and that list is exactly what the right-hand side of [bin]
	// and toy-index's probe resolve against.
	for i, f := range e.files {
		e.files[i] = strings.TrimPrefix(f, prefix)
	}
	return nil
}

// stripKeepsLinksInside reports whether every RELATIVE symlink in the package still
// resolves inside destDir once that top layer is gone.
//
// This is the one escape surface stripping introduces on its own, so stripping has to
// close it itself: removing a layer moves every link's resolution origin up by one.
// Self-contained links, macOS's `Versions/Current -> A` kind, are unaffected. A link
// that first climbs to the package root and then walks back down through the
// top-level directory name is not:
//
//	sub/deep/back -> ../../sub/real.txt    before the strip: dest/sub/real.txt   ok
//	   deep/back  -> ../../sub/real.txt    after the strip:  destDir's PARENT    escape
//
// Such a package is left unstripped and handed to the caller as-is: [bin] then fails
// loudly on a path that does not line up, and that is far better than leaving a link
// inside the package that can read outside destDir (iron rule 7).
// Anything we cannot read - Lstat or Readlink errors - counts as unsafe. Stripping is
// a convenience, never a requirement.
func (e *extractor) stripKeepsLinksInside(prefix string) bool {
	for _, rel := range e.files {
		full := filepath.Join(e.dest, rel)
		fi, err := os.Lstat(full)
		if err != nil {
			return false
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue // regular files and hard links resolve by inode; moving a level changes nothing
		}
		target, err := os.Readlink(full)
		if err != nil {
			return false
		}
		moved := filepath.Join(e.dest, strings.TrimPrefix(rel, prefix))
		if e.checkLinkTarget(moved, target) != nil {
			return false
		}
	}
	return true
}

type extractor struct {
	dest    string          // destination directory, absolute and already Cleaned
	limit   int64           // cap on total extracted bytes
	written int64           // bytes written so far
	entries int             // members seen so far; guards against entry-count bombs
	files   []string        // relative paths returned to the caller (files and links, no dirs)
	created []string        // for cleanup: every entry we really created, in creation order
	regular map[string]bool // regular files this run wrote (absolute); hard links accept only these
}

// note records one member. MaxTotalBytes counts bytes, not inodes: a few dozen MB of
// zip can hold millions of empty entries, and that unpacks into millions of files.
func (e *extractor) note() error {
	e.entries++
	if e.entries > maxEntries {
		return fmt.Errorf("%w: more than %d entries; archive discarded", ErrTooLarge, maxEntries)
	}
	return nil
}

func (e *extractor) run(src string) error {
	switch formatOf(src) {
	case kindZip:
		zr, err := zip.OpenReader(src)
		if err != nil {
			return fmt.Errorf("archive: cannot open zip %s: %w", filepath.Base(src), err)
		}
		defer zr.Close()
		return e.unzip(&zr.Reader)
	case kindTarGz:
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("archive: cannot open %s: %w", filepath.Base(src), err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("archive: %s is not valid gzip: %w", filepath.Base(src), err)
		}
		defer gz.Close()
		return e.untar(gz)
	case kindTarBz2:
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("archive: cannot open %s: %w", filepath.Base(src), err)
		}
		defer f.Close()
		// compress/bzip2 is decompress-only and returns no error here; a corrupt
		// stream surfaces while reading, which untar reports with the member it
		// was on.
		return e.untar(bzip2.NewReader(f))
	default: // kindTar
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("archive: cannot open %s: %w", filepath.Base(src), err)
		}
		defer f.Close()
		return e.untar(f)
	}
}

// within reports whether p is destDir itself or something inside it.
// The prefix comparison MUST carry the separator: without it, dest=/x/dest treats
// /x/destevil as one of its own.
func within(dest, p string) bool {
	return p == dest || strings.HasPrefix(p, dest+string(filepath.Separator))
}

// resolve turns a member name into an absolute path under destDir, and returns the
// relative path alongside it. The check is purely lexical (Clean plus a
// separator-aware prefix comparison) and never looks at what is on disk:
//   - a bare strings.Contains("..") misses "a/../../b" and falsely rejects "..foo"
//   - the prefix comparison needs the separator, or dest=/tmp/x treats /tmp/xevil as
//     one of its own
//
// Backslashes always count as separators: "..\evil" is a legal file name on Unix, but
// no author ever meant to ship that name, while an attacker does.
func (e *extractor) resolve(name string) (full, rel string, err error) {
	n := strings.ReplaceAll(name, `\`, "/")
	if strings.TrimSpace(n) == "" {
		return "", "", fmt.Errorf("%w: an entry has no name", ErrTraversal)
	}
	if strings.HasPrefix(n, "/") || filepath.IsAbs(filepath.FromSlash(n)) {
		return "", "", fmt.Errorf("%w: entry %q is an absolute path", ErrTraversal, name)
	}
	rel = filepath.Clean(filepath.FromSlash(n))
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w: entry %q points outside the destination directory", ErrTraversal, name)
	}
	full = filepath.Join(e.dest, rel)
	if !within(e.dest, full) {
		return "", "", fmt.Errorf("%w: entry %q resolves to %s, which is outside %s", ErrTraversal, name, full, e.dest)
	}
	return full, rel, nil
}

// checkLinkTarget verifies that a link target still resolves inside destDir.
// A relative target resolves against the link's own directory, which is exactly how
// the kernel resolves symlinks.
//
// WARNING: this layer alone is NOT enough, do not treat it as the whole defence. The
// lexical check resolves against the lexical parent, the kernel against the real
// parent, and one level of skew between the two is all it takes to climb out. The
// real backstops are the "no symlink anywhere along the path" rule in mkdirAll (see
// the counter-example there) and the "no backtracking .." rule right below.
func (e *extractor) checkLinkTarget(linkFull, target string) error {
	t := strings.ReplaceAll(target, `\`, "/")
	if strings.TrimSpace(t) == "" {
		return fmt.Errorf("%w: symlink %s has an empty target", ErrTraversal, linkFull)
	}
	if strings.HasPrefix(t, "/") || filepath.IsAbs(filepath.FromSlash(t)) {
		return fmt.Errorf("%w: symlink %s points at absolute path %q", ErrTraversal, filepath.Base(linkFull), target)
	}
	// No backtracking ".." in the target. Leading ".."s climb from the link's own
	// parent, and mkdirAll has already guaranteed that entire parent chain is real
	// directories, so lexical and kernel resolution agree. But once a target steps
	// through a plain name and only then hits "..", that name may be a symlink
	// pointing shallower than itself; Clean cancels the ".." against it, so the
	// lexical result still looks inside while the kernel has already climbed out of
	// destDir (x -> "." followed by "x/.." is exactly this). Real packaging tools
	// never emit redundant a/../b paths, so refusing them costs nothing.
	seenName := false
	for _, seg := range strings.Split(t, "/") {
		switch seg {
		case "", ".":
		case "..":
			if seenName {
				return fmt.Errorf("%w: symlink %s has target %q containing a backtracking '..', so where it resolves cannot be trusted",
					ErrTraversal, filepath.Base(linkFull), target)
			}
		default:
			seenName = true
		}
	}
	full := filepath.Clean(filepath.Join(filepath.Dir(linkFull), filepath.FromSlash(t)))
	if !within(e.dest, full) {
		return fmt.Errorf("%w: symlink %s points at %q, which resolves to %s, outside %s",
			ErrTraversal, filepath.Base(linkFull), target, full, e.dest)
	}
	return nil
}

// safeMode keeps exactly two permission tiers: 0755 if the entry was executable, 0644
// otherwise. NEVER copy the archive's mode through - setuid, setgid and sticky must
// not survive extraction.
func safeMode(m os.FileMode) os.FileMode {
	if m.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func (e *extractor) track(full, rel string, isDir bool) {
	e.created = append(e.created, full)
	if !isDir {
		e.files = append(e.files, rel)
	}
}

func (e *extractor) cleanup() {
	for i := len(e.created) - 1; i >= 0; i-- {
		os.Remove(e.created[i]) // reverse order: contents go first so directories can empty
	}
}

// mkdirAll creates directories one element at a time from destDir down, recording
// every level it actually created so a failure can roll back precisely. We cannot
// just WalkDir and delete, because destDir may already hold someone else's files.
//
// Every element gets its own Lstat and symlinks are refused. THIS is where "never
// follow a link out of bounds" is really enforced: resolve()'s lexical check resolves
// a link target against the LEXICAL parent while the kernel uses the REAL parent, and
// the moment those two depths disagree, the path climbs out. Counter-example - each
// of these three members passes the lexical check on its own:
//
//	x        -> "."   lexically dest/x, within ok
//	x/up     -> ".."  lexically Join(dest/x, "..") = dest, ok; really dest/up -> destDir's parent
//	x/up/pwned        lexically dest/x/up/pwned, ok; really written outside destDir
//
// So "any link we let through must point inside destDir" does not hold by induction,
// and the path has to be checked again as we walk it. This also blocks symlinks that
// were already sitting in destDir before we started.
//
// Do NOT return early when an element already exists and is a directory: Lstat on a
// deep path silently resolves the symlinks in the middle, and the upper elements
// would then never get checked at all.
func (e *extractor) mkdirAll(dir string) error {
	if !within(e.dest, dir) {
		return nil // paths heading outward were already refused by resolve
	}
	rel, err := filepath.Rel(e.dest, dir)
	if err != nil {
		return fmt.Errorf("archive: cannot resolve %s relative to %s: %w", dir, e.dest, err)
	}
	if rel == "." {
		return nil
	}
	cur := e.dest
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if err := os.Mkdir(cur, 0o755); err != nil {
				return fmt.Errorf("archive: cannot create directory %s: %w", cur, err)
			}
			e.created = append(e.created, cur)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s in the path is a symlink; refusing to write through it", ErrTraversal, cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("archive: %s already exists and is not a directory; paths inside the archive conflict", cur)
		}
	}
	return nil
}

func (e *extractor) writeFile(full, rel string, r io.Reader, mode os.FileMode) error {
	if err := e.mkdirAll(filepath.Dir(full)); err != nil {
		return err
	}
	if err := e.removeExisting(full); err != nil {
		return err
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("archive: cannot write %s: %w", rel, err)
	}
	e.track(full, rel, false) // book it first: a half-written file still has to be cleanable

	// Read one byte past the budget to spot an overrun; never trust the declared size.
	remain := e.limit - e.written
	n, copyErr := io.Copy(f, io.LimitReader(r, remain+1))
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("archive: cannot write %s: %w", rel, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("archive: cannot write %s: %w", rel, closeErr)
	}
	if n > remain {
		return fmt.Errorf("%w: extracting %s passed the %d byte limit; archive discarded", ErrTooLarge, rel, e.limit)
	}
	e.written += n

	// OpenFile's mode gets trimmed by umask, so pin the two tiers explicitly.
	if err := os.Chmod(full, mode); err != nil {
		return fmt.Errorf("archive: cannot set permissions on %s: %w", rel, err)
	}
	e.regular[full] = true
	return nil
}

func (e *extractor) writeSymlink(full, rel, target string) error {
	if err := e.checkLinkTarget(full, target); err != nil {
		return err
	}
	if err := e.mkdirAll(filepath.Dir(full)); err != nil {
		return err
	}
	if err := e.removeExisting(full); err != nil {
		return err
	}
	if err := os.Symlink(target, full); err != nil {
		return fmt.Errorf("archive: cannot create symlink %s: %w", rel, err)
	}
	e.track(full, rel, false)
	return nil
}

// writeHardlink's target is a path inside the package and has to land inside destDir
// like everything else.
//
// It must also be A REGULAR FILE THIS EXTRACTION ITSELF WROTE. os.Link follows
// symlinks in the middle of the target path, so with a lexical check alone, one stale
// outward-pointing link already sitting in destDir lets a package hard-link any file
// on the system into itself - the same inode, not a copy. Hard links in tar only ever
// point at members that appeared earlier, so this restriction never hits a real
// package; it also makes a hand-rolled archive whose target appears later fail loudly
// instead of leaving half a package behind.
func (e *extractor) writeHardlink(full, rel, target string) error {
	targetFull, _, err := e.resolve(target)
	if err != nil {
		return fmt.Errorf("bad target for hard link %s: %w", rel, err)
	}
	if !e.regular[targetFull] {
		return fmt.Errorf("%w: hard link %s points at %q, which is not a regular file this archive wrote earlier",
			ErrTraversal, rel, target)
	}
	if err := e.mkdirAll(filepath.Dir(full)); err != nil {
		return err
	}
	if err := e.removeExisting(full); err != nil {
		return err
	}
	if err := os.Link(targetFull, full); err != nil {
		return fmt.Errorf("archive: cannot create hard link %s: %w", rel, err)
	}
	e.track(full, rel, false)
	e.regular[full] = true // once written, a hard link is itself a regular file and can be linked again
	return nil
}

// removeExisting handles the same name appearing twice in one archive: writing over it
// in place would follow the older symlink.
func (e *extractor) removeExisting(full string) error {
	fi, err := os.Lstat(full)
	if err != nil || fi.IsDir() {
		return nil
	}
	if err := os.Remove(full); err != nil {
		return fmt.Errorf("archive: cannot remove the existing %s: %w", full, err)
	}
	return nil
}

func (e *extractor) untar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: cannot read tar: %w", err)
		}
		if err := e.note(); err != nil {
			return err
		}
		full, rel, err := e.resolve(hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := e.mkdirAll(full); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeGNUSparse:
			// A GNU sparse file (`tar --sparse`) is a REGULAR FILE WITH CONTENT,
			// just encoded as sparse segments. Go's reader has already wrapped tr in
			// a sparseFileReader, so reading it yields the fully expanded content -
			// but it KEEPS Typeflag == 'S' for us (only PAX-format sparse files get
			// normalised to TypeReg). Falling into default would silently drop a file
			// while Extract still reported success. Expanded content can be huge, and
			// writeFile's byte limit covers exactly that.
			if err := e.writeFile(full, rel, tr, safeMode(hdr.FileInfo().Mode())); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := e.writeSymlink(full, rel, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := e.writeHardlink(full, rel, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// Device nodes and FIFOs carry no content, so skip them without erroring.
			// In a binary release these are only ever noise the packaging tool swept
			// in, and not worth failing the whole install over.
			continue
		default:
			// Everything else - PAX global headers, GNU volume labels and friends -
			// carries no content either, so skip it the same way. But the moment such
			// an entry DECLARES content it must not be dropped silently: Go's reader
			// decompresses those bytes out of the stream and throws them away, where
			// MaxTotalBytes never sees them (a few KiB of archive can burn hundreds
			// of MiB of decompression), and "a file is missing yet the install
			// succeeded" is far harder to diagnose than a refusal.
			if hdr.Size > 0 {
				return fmt.Errorf("%w: entry %s has unknown type %q yet declares %d bytes of content; refusing to silently drop it",
					ErrUnsupported, rel, hdr.Typeflag, hdr.Size)
			}
			continue
		}
	}
}

func (e *extractor) unzip(zr *zip.Reader) error {
	for _, f := range zr.File {
		if err := e.note(); err != nil {
			return err
		}
		full, rel, err := e.resolve(f.Name)
		if err != nil {
			return err
		}
		mode := f.Mode()
		switch {
		case mode&os.ModeSymlink != 0: // must come before the ModeType case - symlink is part of ModeType
			target, err := readZipLinkTarget(f)
			if err != nil {
				return err
			}
			if err := e.writeSymlink(full, rel, target); err != nil {
				return err
			}
		case f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/"):
			if err := e.mkdirAll(full); err != nil {
				return err
			}
		case mode&os.ModeType != 0:
			continue // device node / FIFO / socket: skip
		default:
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("archive: cannot read zip entry %s: %w", rel, err)
			}
			err = e.writeFile(full, rel, rc, safeMode(mode))
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func readZipLinkTarget(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("archive: cannot read symlink %s: %w", f.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxLinkTargetBytes+1))
	if err != nil {
		return "", fmt.Errorf("archive: cannot read symlink %s: %w", f.Name, err)
	}
	if len(b) > maxLinkTargetBytes {
		return "", fmt.Errorf("%w: %s claims to be a symlink but its target exceeds %d bytes", ErrTraversal, f.Name, maxLinkTargetBytes)
	}
	return string(b), nil
}

// SingleFileCompression reports the compression on a single compressed file --
// not a container, one file squeezed. restic ships
// `restic_0.19.1_linux_amd64.bz2` and nothing else for linux/amd64, and several
// projects ship a bare `.gz` the same way.
//
// Returns "" for a tarball: `.tar.gz` is a container and goes through Extract.
//
// Judged on the NAME, so callers pass the original asset name: a downloaded file
// sits in the cache under its sha256 and carries no suffix at all.
func SingleFileCompression(name string) string {
	n := strings.ToLower(filepath.Base(name))
	if formatOf(n) != kindNone {
		return "" // a container, not a squeezed single file
	}
	switch {
	case strings.HasSuffix(n, ".bz2"):
		return "bz2"
	case strings.HasSuffix(n, ".gz"):
		return "gz"
	}
	return ""
}

// Decompress writes the single file inside src to dst.
//
// The output is capped at MaxTotalBytes exactly as extraction is. A single
// compressed file is the easiest decompression bomb there is -- bzip2 reaches
// ratios in the thousands, so a few hundred kilobytes downloaded and checksummed
// can fill a disk. The cap has to be on bytes WRITTEN, never on any length the
// file claims about itself.
// Decompress writes the single file inside src to dst. name is the original asset
// name -- src is the cache path, which is a bare sha256 with no suffix to read.
func Decompress(src, name, dst string) error { return decompressTo(src, name, dst, MaxTotalBytes) }

// decompressTo carries the limit as a parameter so a test can prove the bomb
// guard with a small budget instead of really writing two gigabytes -- the same
// shape Extract/extractTo use.
func decompressTo(src, name, dst string, limit int64) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("archive: cannot open %s: %w", src, err)
	}
	defer f.Close()

	var r io.Reader
	switch SingleFileCompression(name) {
	case "bz2":
		r = bzip2.NewReader(f)
	case "gz":
		zr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("archive: %s is not valid gzip: %w", src, err)
		}
		defer zr.Close()
		r = zr
	default:
		return fmt.Errorf("%w: %s carries no compression this can undo", ErrUnsupported, name)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("archive: cannot write %s: %w", dst, err)
	}
	n, cerr := io.Copy(out, io.LimitReader(r, limit+1))
	closeErr := out.Close()
	if cerr != nil {
		os.Remove(dst)
		return fmt.Errorf("archive: cannot unpack %s: %w", src, cerr)
	}
	if closeErr != nil {
		os.Remove(dst)
		return fmt.Errorf("archive: cannot write %s: %w", dst, closeErr)
	}
	if n > limit {
		os.Remove(dst)
		return fmt.Errorf("%w: %s expands past the %d byte limit; the partial file was deleted. "+
			"The count is the bytes actually written, not anything the file claims about itself",
			ErrTooLarge, src, limit)
	}
	// O_CREATE's mode is filed down by umask, and this file has to be runnable.
	return os.Chmod(dst, 0o755)
}
