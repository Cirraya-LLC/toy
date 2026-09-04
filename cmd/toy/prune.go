package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/Cirraya-LLC/toy/internal/ui"
)

// runPrune reclaims the download cache.
//
// uninstall deliberately leaves the bytes behind: they are content-addressed, and
// the cache directory is exactly what `toy serve` mirrors (spec §8). That is a
// good reason to keep some of them. It was never a reason to keep every version
// ever downloaded forever, with no command to reclaim them and no line anywhere
// telling the user how much they add up to -- which is what this fixes.
//
// The default keeps whatever an installed package still refers to. Those are the
// ones a reinstall would otherwise re-download, and they are the ones worth
// mirroring.
func runPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	all := fs.Bool("all", false, "delete every cached download, including the ones installed packages use")
	dry := fs.Bool("dry-run", false, "print what would be deleted and delete nothing")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errUsage
	}

	st, _, err := openStore()
	if err != nil {
		return err
	}
	es, err := st.CacheEntries()
	if err != nil {
		return err
	}

	var keepN, dropN int
	var keepB, dropB int64
	for _, e := range es {
		if e.InUse && !*all {
			keepN, keepB = keepN+1, keepB+e.Bytes
			continue
		}
		dropN, dropB = dropN+1, dropB+e.Bytes
	}

	if dropN == 0 {
		ui.Info("nothing to reclaim: the cache holds %s in %s.", prnSize(keepB), prnCount(keepN, "download"))
		return nil
	}
	if *dry {
		ui.Info("%s in %s would be deleted, %s in %s kept.",
			prnSize(dropB), prnCount(dropN, "download"), prnSize(keepB), prnCount(keepN, "download"))
		ui.Info("Run `toy prune` without --dry-run to do it.")
		return nil
	}

	removed, freed, err := st.PruneCache(*all)
	if err != nil {
		return err
	}
	ui.Info("reclaimed %s from %s.", prnSize(freed), prnCount(removed, "cached download"))
	if keepN > 0 {
		ui.Info("Kept %s still used by installed packages.", prnCount(keepN, "download"))
	}
	// Say the cost out loud rather than leaving the user to discover it: these are
	// the bytes `toy serve` was handing out.
	ui.Info("`toy serve` mirrors this directory, so it now has that much less to offer.")
	return nil
}

func prnSize(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	d, e := int64(u), 0
	for m := n / u; m >= u; m /= u {
		d *= u
		e++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(d), "KMGTP"[e])
}

func prnCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
