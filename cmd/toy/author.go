package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// runKeygen generates a key pair into ~/.toy/keys and prints the public key and its
// fingerprint. It refuses to overwrite an existing one.
func runKeygen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errUsage
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	kp, err := sign.Generate()
	if err != nil {
		return err
	}
	if err := sign.SaveKeyPair(st.KeysDir(), kp); err != nil {
		// SaveKeyPair has already named the file that stopped it; what gets added
		// here is why there is no --force. Overwriting the private key means losing
		// that name for good — nothing can ever re-sign the entries the old key
		// signed, and the moment the key field in the index changes, everyone who
		// knew the old key sees a key change.
		return fmt.Errorf("%w. toy offers no overwrite flag: swapping keys means giving up every name the old "+
			"key claimed, and nobody can re-sign the entries it signed — if you really want to start over, "+
			"move that file aside yourself", err)
	}

	ui.Info("Key pair written to %s (key.sec is mode 0600 — do not copy it anywhere, do not commit it)", st.KeysDir())
	ui.Info("Fingerprint %s — this is the string other people see when they install", sign.Fingerprint(kp.Public))
	ui.Info("Next: paste the public key below into the \"key\" field of an index entry — that is how you claim a name.")
	ui.Info("Running `toy publish` in the project directory writes the whole entry, key included.")
	// Machine-readable goes to stdout (spec §9), so `toy keygen | pbcopy` gets the
	// bare public key and nothing else.
	fmt.Println(hex.EncodeToString(kp.Public))
	return nil
}

// autSignMessage is the byte string an author's signature covers. THIS DEFINITION IS
// FROZEN AND MUST NOT CHANGE:
//
//	id + "\n" + source + "\n" + the verbatim bytes of toy.toml
//
// Change one byte and every signature already out there stops verifying on the same
// day, while what the user sees is "the signature does not verify" — a sentence that
// reads like the author's private key leaked, not like we changed a format. The only
// way to ever redefine it is a new, versioned field, with both kinds of signature
// coexisting for a while.
//
// Verbatim bytes rather than the parsed Toyfile: a signature has to cover "the file
// the user actually downloads", and re-serializing hands out a chance to compute one
// signature that is valid over two different texts.
func autSignMessage(id, source string, toyTOML []byte) []byte {
	msg := make([]byte, 0, len(id)+len(source)+len(toyTOML)+2)
	msg = append(msg, id...)
	msg = append(msg, '\n')
	msg = append(msg, source...)
	msg = append(msg, '\n')
	return append(msg, toyTOML...)
}

// runPublish reads the toy.toml in a directory, validates it, signs it with the local
// private key, and prints an entry that can be pasted straight into an index JSON.
// It goes nowhere online and commits nothing — a v1 index is a local file, and
// pasting is the whole workflow (spec §5).
//
// --inline is a different job, and it is FOR AN INDEX MAINTAINER: writing packaging
// instructions for a repository you do not own. That toy.toml goes verbatim into the
// entry's toyfile field, clients only use it when the upstream repository really has
// none of its own, and it installs with a reduced-trust marker
// (store.TrustIndexed). It is how the index gets off the ground at all — of the
// thousands of CLI tools out there, not one has a toy.toml.
func runPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	source := fs.String("source", "", "the entry's source, like github:owner/repo; inferred from the git origin if omitted")
	inline := fs.Bool("inline", false, "inline this toy.toml verbatim into the entry: packaging instructions for a repository that is NOT yours (it steps aside the moment upstream ships its own toy.toml)")
	// Borrow install's parsing: only that makes -h print the two descriptions above,
	// and --inline versus an ordinary publish are two different jobs whose difference
	// is written down nowhere else.
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errUsage
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	path := filepath.Join(dir, "toy.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w. publish reads the toy.toml in this directory — "+
			"eight lines is enough (name / [bin] / [artifact]), and nobody has to approve it", path, err)
	}
	// Validate before signing: signing a toy.toml that will not even install just
	// defers the error onto somebody else's machine.
	tf, err := toyfile.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not ready to publish: %w", path, err)
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}

	src := strings.TrimSpace(*source)
	if src == "" {
		if src, err = autGitSource(ctx, dir, st.BinDir(), cfg.Hosts); err != nil {
			return err
		}
	}
	r, err := ref.Parse(src)
	if err != nil {
		return fmt.Errorf("the source %q does not parse: %w", src, err)
	}
	if r.Kind != ref.KindHost {
		return fmt.Errorf("the source has to look like github:owner/repo (the prefix can be gitlab: / codeberg: "+
			"or any host you added in ~/.toy/config.toml), got %q. A bare owner/repo would send the client back "+
			"to an index lookup, which is a loop; and an entry pointing straight at a toy.toml URL still has to "+
			"be written by hand, because that form carries no owner/repo to use as the id", src)
	}
	if r.Version != "" {
		return fmt.Errorf("leave the @%s off the source: an index entry names a repository, and which version "+
			"gets installed is decided by the git tags. Pin a version here and you have to come back and edit "+
			"the index on every release", r.Version)
	}
	if tf.Desc == "" {
		ui.Warn("toy.toml has no desc, so the entry's description will be empty — and that is mostly what `toy search` searches")
	}

	entry := index.Entry{
		ID:     r.ID(),
		Source: r.String(),
		Desc:   tf.Desc,
	}
	if *inline {
		// An inline entry has to be VERSION-INDEPENDENT: it lives in the index and
		// follows whatever tag git ls-remote finds upstream, while [sha256] pins the
		// bytes of one specific version. Leave it in and upstream's next release
		// hands every user "checksum mismatch, refusing to install" — which reads
		// exactly like a poisoning warning when all that happened is the author cut
		// a normal release. So it gets stripped here.
		//
		// The stripping has to happen BEFORE signing: what is signed is the verbatim
		// text that goes inline (autSignMessage).
		// (Your own repository is unaffected: there the toy.toml ships on the same
		// tag as the assets, so the hashes are correct and belong there. Only an
		// entry written on somebody else's behalf must not keep them.)
		if stripped, ok := autStripSHA256(raw); ok {
			raw = stripped
			ui.Warn("Stripped [sha256] from the inlined toy.toml: it pins the bytes of one specific version,")
			ui.Info("while an inline entry follows whatever tag upstream publishes next. Left in, the author's")
			ui.Info("next release would hand everyone a checksum mismatch. First installs are still pinned by TOFU.")
		}
		// Verbatim text goes into the entry: what the client verifies the signature
		// against is exactly this text (autSignMessage), and re-serializing means a
		// different piece of text (same reasoning as the comment in rsvGetToyfile).
		entry.Toyfile = string(raw)
		ui.Warn("This is an INLINE entry: you are writing packaging instructions for %s, which is not your repository.", entry.Source)
		ui.Info("Whoever installs it sees a reduced-trust line: the binary comes from the author's release, but")
		ui.Info("which asset and what the command is called come from the index — so write the [bin] names")
		ui.Info("the way the author meant them. The day upstream adds its own toy.toml, clients switch to it")
		ui.Info("and this inline entry steps aside on its own.")
	}

	// Signing is an optional upgrade, not an entry ticket (spec §4): with no key pair
	// it still prints the entry, and that entry can still be listed in an index.
	switch kp, err := sign.LoadKeyPair(st.KeysDir()); {
	case errors.Is(err, sign.ErrNoKey):
		ui.Info("There is no key pair on this machine, so this entry is unsigned — it can still be listed and still installs.")
		ui.Info("Run `toy keygen` to make one and claim the name %s; after that publish adds the key and sig for you.", entry.ID)
	case err != nil:
		return err
	default:
		entry.Key = hex.EncodeToString(kp.Public)
		entry.Sig = sign.Sign(kp.Private, autSignMessage(entry.ID, entry.Source, raw))
		ui.Info("Signed with %s (the signature covers id + source + the verbatim bytes of this toy.toml)", sign.Fingerprint(kp.Public))
		if *inline {
			// The inline level never reaches "✓ signed" (rule 2): the signer is
			// whoever wrote this record, not the author. Say so here, or a
			// maintainer will take a signature for an endorsement.
			ui.Info("A signature on an inline entry only proves this record was not altered. It will NOT make it")
			ui.Info("show up as \"✓ signed\" — that check mark means the author signed, and you wrote these instructions.")
		}
	}

	out, err := json.MarshalIndent(entry, "  ", "  ")
	if err != nil {
		return fmt.Errorf("cannot render the index entry: %w", err)
	}
	// The entry goes to stdout and the explanations to stderr (spec §9), so
	// `toy publish > entry.json` gets nothing but JSON.
	fmt.Printf("  %s\n", out)
	ui.Info("Paste the block above into the packages array of an index JSON and you have published (add a line")
	ui.Info("like \"tags\": [\"web\"] yourself if you want tags). toy does not go online for you and does not commit")
	ui.Info("for you — an index is just a JSON file, and anyone who forks one has a parallel index.")
	return nil
}

// autGitSource infers an index entry's source from git's origin.
// binDir is toy's own bin directory, which autGitPath needs in order to exclude it.
func autGitSource(ctx context.Context, dir, binDir string, overrides map[string]host.Host) (string, error) {
	remote, err := autGitRemote(ctx, dir, binDir)
	if err != nil {
		return "", err
	}
	domain, owner, repo, err := autParseRemote(remote)
	if err != nil {
		return "", err
	}
	name := autHostName(domain, overrides)
	if name == "" {
		return "", fmt.Errorf("origin points at %s, and that domain is not in toy's host table. "+
			"Either name it directly (--source github:%s/%s), or add a [hosts.<name you pick>] section "+
			"in ~/.toy/config.toml with the four templates: raw / clone / release / web",
			domain, owner, repo)
	}
	return name + ":" + owner + "/" + repo, nil
}

// autGitPath finds the absolute path of git, SKIPPING TOY'S OWN BIN DIRECTORY.
//
// Handing a bare "git" to exec runs the first git on PATH, and toy's bin directory is
// on PATH by documented design — so any installed package that ships a command named
// git gets executed by `toy publish`. That is a hole straight through iron law 1, not
// an inconvenience.
//
// This is a second implementation of lookupGit in internal/host (which has no usable
// exported entry point yet). THE RULES IN THE TWO PLACES MUST STAY IDENTICAL, word
// for word: walk PATH, skip relative entries, skip ~/.toy/bin, take the first
// runnable regular file. Relax this side by one notch and publish becomes the way
// around it. The day internal/host exports an entry point, move this and qryCheckGit
// over to it and delete this copy.
func autGitPath(binDir string) (string, error) {
	var inToyBin string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		// Relative entries ("." and the empty entry that turns up everywhere in
		// PATH) mean whatever directory the process happens to be in. Running toy
		// inside a directory somebody else can write to must not amount to handing
		// over iron law 1.
		if !filepath.IsAbs(dir) {
			continue
		}
		cand := filepath.Join(dir, "git")
		// Follow symlinks: bin/ holds nothing but symlinks, and the real regular
		// file lives under pkg.
		if fi, err := os.Stat(cand); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		if binDir != "" && autSameDir(dir, binDir) {
			inToyBin = cand // kept only so the error can be specific; keep looking for a real git
			continue
		}
		return cand, nil
	}
	if inToyBin != "" {
		return "", fmt.Errorf("refusing to run it: the only git on PATH is %s, which lives in toy's own bin "+
			"directory — meaning it came from a package somebody installed. Running it would hand that package "+
			"execution, and toy never executes anything from a package. This is very likely an attack. "+
			"Use `toy list` to find which package put it there, remove it, and install a real git; "+
			"or pass --source github:owner/repo, which does not touch git at all", inToyBin)
	}
	return "", errors.New("no git command found, so the source cannot be inferred. Pass --source github:owner/repo instead")
}

// autSameDir / autCanonDir: absolute path first, then resolve symlinks (/usr/bin is
// often a symlink to /bin), falling back to string comparison when that fails. The
// same thing as the identically named functions in internal/host and internal/store.
func autSameDir(a, b string) bool { return autCanonDir(a) == autCanonDir(b) }

func autCanonDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// autGitRemote reads this directory's origin URL.
// One line of config, nothing over the network, and nothing from the repository is
// executed (iron law 1).
func autGitRemote(ctx context.Context, dir, binDir string) (string, error) {
	gitPath, err := autGitPath(binDir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, gitPath, "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("%s is not a git repository, or it has no remote called origin, so there is no "+
			"way to tell where this package lives. Pass --source github:owner/repo instead", dir)
	}
	remote := strings.TrimSpace(string(out))
	if remote == "" {
		return "", errors.New("the origin URL is empty. Pass --source github:owner/repo instead")
	}
	return remote, nil
}

// autParseRemote splits an origin URL into domain + owner + repo. Two shapes are
// recognized:
//
//	https://github.com/owner/repo.git   (scheme form; ssh:// and git:// come here too)
//	git@github.com:owner/repo.git       (scp form, no scheme)
//
// It splits and does not validate characters — the source it builds goes through
// ref.Parse, and that is where the allowlist lives.
func autParseRemote(remote string) (domain, owner, repo string, err error) {
	s := strings.TrimSpace(remote)
	var hostPart, path string
	switch {
	case strings.Contains(s, "://"):
		u, perr := url.Parse(s)
		if perr != nil {
			return "", "", "", fmt.Errorf("the origin %q is not a valid URL: %w. Pass --source github:owner/repo instead", s, perr)
		}
		hostPart, path = u.Hostname(), u.Path
	case strings.Contains(s, ":") && !strings.HasPrefix(s, "/"):
		// scp form: [user@]host to the left of the colon, path to the right.
		hostPart, path, _ = strings.Cut(s, ":")
		if _, after, ok := strings.Cut(hostPart, "@"); ok {
			hostPart = after
		}
	default:
		// An origin that is a local path (/srv/git/x.git and the like) has no
		// domain, so there is no host to infer.
		return "", "", "", fmt.Errorf("the origin is %q, which does not say which host it is — a remote that is "+
			"a local path has no domain. Pass --source github:owner/repo instead", s)
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	segs := strings.Split(path, "/")
	if hostPart == "" || len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		// GitLab subgroups (a/b/c) land here: a ref has exactly one owner/repo
		// level, so it cannot be installed, and inferring a wrong source is worse
		// than saying so (same reasoning as iron law 8).
		return "", "", "", fmt.Errorf("cannot make out owner/repo from the origin %q — a toy package reference "+
			"has exactly one level of owner/repo, and deeper paths like GitLab subgroups are not supported yet. "+
			"Pass --source github:owner/repo instead", s)
	}
	return strings.ToLower(hostPart), segs[0], segs[1], nil
}

// autHostName looks a host prefix up by domain (github.com -> github).
// It searches the host table itself, so a self-hosted instance the user added in
// config.toml is recognized automatically — hosts are configuration, not code, and
// this is not the place for a second copy of the domain list.
func autHostName(domain string, overrides map[string]host.Host) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return ""
	}
	// The user's own config wins (they may have overridden the built-in github). When
	// several entries share a domain, sort by name and take the first: map iteration
	// order is random, and without sorting, two publishes of the same repository can
	// print two different sources.
	for _, table := range []map[string]host.Host{overrides, host.Builtin()} {
		for _, name := range slices.Sorted(maps.Keys(table)) {
			if autTemplateHost(table[name].Clone) == domain {
				return strings.ToLower(name)
			}
		}
	}
	return ""
}

// autTemplateHost extracts the domain from a URL template. The {owner} and friends in
// a template do not affect the hostname part.
func autTemplateHost(tmpl string) string {
	u, err := url.Parse(strings.TrimSpace(tmpl))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// autShutdownGrace is how long in-flight requests get to finish after a Ctrl-C.
// With somebody halfway through a 200 MB package, not granting these few seconds
// means cutting their download off.
const autShutdownGrace = 5 * time.Second

// runServe serves ~/.toy/cache as a static directory over HTTP (each file is named
// after its own sha256). Because the content is address-by-hash, this mirror does not
// need to be trusted at all. Spec §8.
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on. The default is local-only; use 0.0.0.0:8080 to serve others")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errUsage
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}

	// Listen before Serve: a port already in use has to fail right here, rather than
	// leaving the user curling a service that does not exist while the screen says
	// it started.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", *addr, err)
	}
	shown := ln.Addr().String()
	if autWildcard(*addr) {
		ui.Warn("Serving on every interface (%s). Every file in the cache is named after its own sha256, so "+
			"the contents cannot be swapped — but you no longer control who downloads what. Think about that "+
			"before running this on a public network", shown)
		if _, port, perr := net.SplitHostPort(shown); perr == nil {
			shown = "your-machine-address:" + port
		}
	}

	ui.Info("Mirror is up, serving %s", st.CacheDir())
	ui.Info("Other people use it by putting this in their ~/.toy/config.toml:")
	ui.Info("")
	ui.Info("    mirrors = [\"http://%s\"]", shown)
	ui.Info("")
	ui.Info("A mirror is only ever consulted when toy.toml declares [sha256] — without a hash there is no way")
	ui.Info("to check that a mirror handed back the right thing.")
	ui.Info("Ctrl-C to stop.")

	srv := &http.Server{
		Handler: autMirrorHandler(st.CacheDir()),
		// A deadline on reading headers only: the body is a package of a few hundred
		// megabytes, and a WriteTimeout would be a death sentence for slow clients.
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("the mirror stopped: %w", err)
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), autShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			ui.Warn("downloads were still in flight and got cut off: %v", err)
		}
		return ctx.Err() // main prints it as "cancelled" and exits 130
	}
}

// autMirrorHandler does exactly one thing: GET/HEAD a 64-character hex path, return
// that file from the cache.
//
// Deliberately not http.FileServer: that lists directories and follows arbitrary
// paths, while this service may be exposed to the internet and the cache directory
// may not always hold nothing but hash-named files. The path has to be exactly a
// sha256; everything else is a 404.
func autMirrorHandler(cacheDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "this mirror only supports GET and HEAD", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if !autIsSHA256(name) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(cacheDir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || fi.IsDir() {
			http.NotFound(w, r)
			return
		}
		// Setting Content-Type ourselves stops ServeContent from sniffing: the
		// cache holds nothing but binary packages, and having one sniffed as
		// text/html only invites browsers and middleboxes to improvise.
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", fi.ModTime(), f) // Range support for free
	})
}

// autIsSHA256 reports whether a string is 64 lowercase hex characters. That is the
// shape of a cache file name (fetch writes them).
func autIsSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// autWildcard reports whether a listen address is open on every interface.
func autWildcard(addr string) bool {
	h, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		h = strings.TrimSpace(addr)
	}
	h = strings.Trim(h, "[]")
	switch h {
	case "", "0.0.0.0", "::", "*":
		return true
	}
	return false
}

// autStripSHA256 removes the whole [sha256] section from the verbatim toy.toml,
// leaving every other byte untouched. Only used on the --inline route; the reason is
// at the call site. It reports whether anything was actually removed.
//
// Written by hand rather than Unmarshal-then-Marshal: what a client verifies the
// signature against is this verbatim text, and re-serializing would replace the
// comments, the ordering and the whitespace — a different piece of text.
func autStripSHA256(raw []byte) ([]byte, bool) {
	lines := strings.Split(string(raw), "\n")
	head := -1 // the [sha256] line itself
	for i, l := range lines {
		if strings.TrimSpace(l) == "[sha256]" {
			head = i
			break
		}
	}
	if head < 0 {
		return raw, false
	}
	start := head
	// Swallow the comments and blank lines immediately above it — those lines are
	// about the sha256, and leaving them behind turns them into an explanation
	// pointing at a section that no longer exists. Stop at anything else.

	for start > 0 {
		prev := strings.TrimSpace(lines[start-1])
		if prev == "" || strings.HasPrefix(prev, "#") {
			start--
			continue
		}
		break
	}
	end := len(lines)
	for i := head + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}
	out := append(append([]string{}, lines[:start]...), lines[end:]...)
	return []byte(strings.Join(out, "\n")), true
}
