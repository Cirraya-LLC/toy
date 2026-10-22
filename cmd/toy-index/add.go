package main

// add: put a hand-written inline toyfile into the index.
//
// gen refuses some repositories with "write this entry by hand": a release that ships
// two programs, asset names inference cannot read, a command name that would collide.
// Writing it by hand used to mean escaping a whole TOML file into a JSON string. This
// takes the file as it is and holds it to everything gen holds a generated one to.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

type addConfig struct {
	id      string
	toyfile []byte
	out     string
	update  bool
	prov    provider
	host    host.Host
	lists   lists
}

func runAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tf := fs.String("toyfile", "", "")
	out := fs.String("index", filepath.Join("index", "toy-index.json"), "")
	update := fs.Bool("update", false, "")
	token := fs.String("token", "", "")
	licenses := fs.String("licenses", filepath.Join("index", "licenses.txt"), "")
	delisted := fs.String("delisted", filepath.Join("index", "delisted.txt"), "")
	declined := fs.String("declined", filepath.Join("index", "declined.txt"), "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *tf == "" {
		return errUsage
	}
	data, err := os.ReadFile(*tf)
	if err != nil {
		return err
	}
	tok := *token
	if tok == "" {
		tok = os.Getenv("GITHUB_TOKEN")
	}
	ls, err := loadLists(*licenses, *delisted, *declined)
	if err != nil {
		return err
	}
	gh, err := host.Lookup("github", nil)
	if err != nil {
		return err
	}
	return add(ctx, addConfig{
		id: fs.Arg(0), toyfile: data, out: *out, update: *update,
		prov: newGitHub("", tok), host: gh, lists: ls,
	})
}

func add(ctx context.Context, cfg addConfig) error {
	r, err := ref.Parse(cfg.id)
	if err != nil || r.Kind != ref.KindIndex || r.Version != "" {
		return fmt.Errorf("%q is not owner/repo", cfg.id)
	}
	id := r.ID()
	switch {
	case cfg.lists.delisted[foldRepo(id)]:
		return fmt.Errorf("%s was delisted at its maintainer's request (index/delisted.txt); it is not listed again", id)
	case cfg.lists.declined[foldRepo(id)]:
		return fmt.Errorf("%s is in index/declined.txt; if a hand-written entry is the answer to why it was declined, remove that line first", id)
	}

	// The same rules as anything else inlined into the index (§5.1 rule 4) -- and the
	// two gen adds on top for an entry that has to survive the next release.
	tf, err := toyfile.ParseFrom(cfg.toyfile, toyfile.OriginIndex)
	if err != nil {
		return err
	}
	if len(tf.SHA256) > 0 {
		return errors.New("an inline entry must not carry [sha256]: it follows upstream's newest tag, so pinning " +
			"one version's bytes turns the next release into a checksum mismatch that reads like a poisoning warning")
	}
	for name := range tf.Bin {
		if w := assetname.PlatformWord(name); w != "" {
			return fmt.Errorf("the command name %q carries the platform word %q; it would only be right on one machine", name, w)
		}
	}

	idx, err := loadIndex(cfg.out)
	if err != nil {
		return err
	}
	if old, ok := idx.Find(id); ok {
		switch {
		case old.Key != "" || old.Sig != "":
			return fmt.Errorf("%s is signed in the index; replacing its toyfile would invalidate the signature", id)
		case old.Toyfile == "":
			return fmt.Errorf("%s is a pointer entry (upstream ships its own toy.toml); inlining one would downgrade it", id)
		case !cfg.update:
			return fmt.Errorf("%s is already in the index; pass --update to replace its toyfile", id)
		}
	}

	info, err := cfg.prov.Fetch(ctx, r.Owner, r.Repo)
	if err != nil {
		return err
	}
	lic, err := decideLicence(id, info.License, info.LicState, cfg.lists.reviewed)
	if err != nil {
		return err
	}
	if tf.License != "" && !strings.EqualFold(tf.License, lic) {
		return fmt.Errorf("the toyfile says license = %q, but the repository's licence is %s", tf.License, lic)
	}
	switch up, err := upstreamToyfile(ctx, cfg.host, r.Owner, r.Repo, info.Tag); {
	case err != nil:
		return err
	case up != nil:
		return fmt.Errorf("upstream ships its own toy.toml at %s; add a pointer entry (id, source, desc) instead", info.Tag)
	}

	idx.Upsert(index.Entry{
		ID:      id,
		Source:  "github:" + r.Owner + "/" + r.Repo,
		Desc:    tomlSafe(info.Desc, descMax),
		Stars:   info.Stars,
		Toyfile: string(cfg.toyfile),
	})
	if err := writeIndex(cfg.out, idx); err != nil {
		return err
	}
	ui.Info("added %s to %s (licence %s). Check it installs: make index-check ARGS=\"--only %s\"", id, cfg.out, lic, id)
	return nil
}
