package main

// decline / delist / drop: take entries out of the index, and record whether they may
// come back. Reviewing a discovery pull request comes down to these three, so they are
// one command each instead of hand-editing JSON and remembering the second file.
//
//   decline  not a fit (a GUI app, a library, the wrong program): never proposed again
//   delist   its maintainer asked: never proposed again, and the promise behind the
//            delist form depends on that
//   drop     not now (too new, broken this week): discovery may propose it again

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/ui"
)

func runRemove(_ context.Context, mode string, args []string) error {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	idxPath := fs.String("index", filepath.Join("index", "toy-index.json"), "")
	reason := fs.String("reason", "", "")
	list := fs.String("list", filepath.Join("index", mode+".txt"), "")
	if mode == "decline" {
		*list = filepath.Join("index", "declined.txt")
	}
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		return errUsage
	}
	if mode != "drop" && strings.TrimSpace(*reason) == "" {
		// The file is read by the next person wondering why a popular tool is missing.
		return fmt.Errorf("%s needs --reason: the next person to wonder why it is not listed reads it", mode)
	}

	ids := make([]string, 0, fs.NArg())
	for _, a := range fs.Args() {
		if _, err := listID(a); err != nil {
			return err
		}
		ids = append(ids, strings.TrimSpace(a))
	}

	idx, err := loadIndex(*idxPath)
	if err != nil {
		return err
	}
	removed := 0
	for _, id := range ids {
		if idx.Remove(id) {
			removed++
			ui.Info("removed %s from %s", id, *idxPath)
		} else {
			ui.Info("%s is not in %s", id, *idxPath)
		}
	}
	if removed > 0 {
		if err := writeIndex(*idxPath, idx); err != nil {
			return err
		}
	}
	if mode == "drop" {
		return nil
	}
	return appendToList(*list, ids, *reason)
}

// appendToList adds the ids not already in the file, one per line, with the reason and
// today's date as the comment.
func appendToList(path string, ids []string, reason string) error {
	have := map[string]bool{}
	if err := loadRepoSet(path, have); err != nil {
		return err
	}
	var b strings.Builder
	for _, id := range ids {
		if have[foldRepo(id)] {
			ui.Info("%s is already in %s", id, path)
			continue
		}
		have[foldRepo(id)] = true
		fmt.Fprintf(&b, "%s  # %s (%s)\n", id, strings.Join(strings.Fields(reason), " "), time.Now().UTC().Format("2006-01-02"))
	}
	if b.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ui.Info("recorded in %s", path)
	return nil
}
