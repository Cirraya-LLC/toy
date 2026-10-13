package main

// Which licences let a project into the index, and the two hand-kept lists next to it.
//
// toy never rehosts anything -- every download goes to the author's own release -- so
// strictly, listing a project needs no permission from its licence. The bar is higher
// on purpose: the index is a recommendation people install from, and `toy serve` lets
// anyone re-serve what they downloaded, which IS redistribution. So an entry has to
// carry a licence that allows using and redistributing the binaries: OSI-approved, or
// a public-domain dedication. Source-available licences (BUSL, SSPL, Elastic,
// PolyForm, Commons Clause) are out, and so is no licence at all, which means all
// rights reserved.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/ref"
)

// openLicenses are the SPDX identifiers GitHub's licence detection can report that
// allow use and redistribution. Copyleft is fine: nothing is modified or linked.
var openLicenses = map[string]bool{
	"0BSD": true, "AFL-3.0": true, "AGPL-3.0": true, "Apache-2.0": true,
	"Artistic-2.0": true, "BSD-2-Clause": true, "BSD-3-Clause": true,
	"BSD-3-Clause-Clear": true, "BSL-1.0": true, "CC0-1.0": true, "ECL-2.0": true,
	"EPL-1.0": true, "EPL-2.0": true, "EUPL-1.1": true, "EUPL-1.2": true,
	"GPL-2.0": true, "GPL-3.0": true, "ISC": true, "LGPL-2.1": true, "LGPL-3.0": true,
	"LPPL-1.3c": true, "MIT": true, "MIT-0": true, "MPL-2.0": true, "MS-PL": true,
	"MS-RL": true, "MulanPSL-2.0": true, "NCSA": true, "OSL-3.0": true,
	"PostgreSQL": true, "UPL-1.0": true, "Unlicense": true, "Vim": true,
	"WTFPL": true, "Zlib": true,
}

// licenceState is what GitHub said about a repo's licence, before anyone looked.
type licenceState int

const (
	licenceKnown        licenceState = iota // an SPDX identifier
	licenceNone                             // no licence file at all
	licenceUnrecognized                     // a licence file GitHub could not match ("other" / NOASSERTION)
)

var (
	errNoLicence         = errors.New("no licence")
	errUnrecognized      = errors.New("licence not recognized")
	errNotOpen           = errors.New("licence is not open source")
	errDelistedByRequest = errors.New("delisted")
)

// decideLicence returns the SPDX identifier an entry gets, or why it gets none.
//
// reviewed is index/licenses.txt: a person read the licence of a repo GitHub could not
// identify and wrote down what it is. That record names the licence; it does not
// override the allowlist -- a reviewed BUSL is still BUSL.
func decideLicence(id, spdx string, st licenceState, reviewed map[string]string) (string, error) {
	if st != licenceKnown {
		if r, ok := reviewed[foldRepo(id)]; ok {
			spdx = r
		} else if st == licenceNone {
			return "", fmt.Errorf("%w: the repository has no licence file, which means all rights reserved", errNoLicence)
		} else {
			return "", fmt.Errorf("%w: GitHub cannot identify its licence file. Read it, and if it allows "+
				"redistribution add `%s <SPDX-id>  # where you read it` to index/licenses.txt", errUnrecognized, id)
		}
	}
	if !openLicenses[spdx] {
		return "", fmt.Errorf("%w: %s is not on the open-source allowlist (cmd/toy-index/license.go)", errNotOpen, tomlSafe(spdx, 64))
	}
	return spdx, nil
}

// lists holds the hand-kept files. All are keyed by folded owner/repo, the same way the
// index compares ids.
type lists struct {
	reviewed map[string]string // index/licenses.txt: owner/repo -> SPDX, read by a person
	delisted map[string]bool   // index/delisted.txt: never list again (the maintainer asked)
	declined map[string]bool   // index/declined.txt: a reviewer decided it does not belong
}

// loadLists reads the files. A missing file is an empty list; a malformed line is an
// error, because a typo in delisted.txt would quietly re-list somebody who asked to go.
func loadLists(licensesPath, delistedPath, declinedPath string) (lists, error) {
	l := lists{reviewed: map[string]string{}, delisted: map[string]bool{}, declined: map[string]bool{}}
	err := eachListLine(licensesPath, func(n int, f []string) error {
		if len(f) != 2 {
			return fmt.Errorf("%s:%d: want `owner/repo SPDX-id`", licensesPath, n)
		}
		id, err := listID(f[0])
		if err != nil {
			return fmt.Errorf("%s:%d: %w", licensesPath, n, err)
		}
		l.reviewed[id] = f[1]
		return nil
	})
	if err != nil {
		return l, err
	}
	if err := loadRepoSet(delistedPath, l.delisted); err != nil {
		return l, err
	}
	return l, loadRepoSet(declinedPath, l.declined)
}

func loadRepoSet(path string, into map[string]bool) error {
	return eachListLine(path, func(n int, f []string) error {
		if len(f) != 1 {
			return fmt.Errorf("%s:%d: want one owner/repo per line", path, n)
		}
		id, err := listID(f[0])
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, n, err)
		}
		into[id] = true
		return nil
	})
}

func eachListLine(path string, fn func(n int, fields []string) error) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range strings.Split(string(data), "\n") {
		if j := strings.IndexByte(line, '#'); j >= 0 {
			line = line[:j]
		}
		if f := strings.Fields(line); len(f) > 0 {
			if err := fn(i+1, f); err != nil {
				return err
			}
		}
	}
	return nil
}

func listID(s string) (string, error) {
	r, err := ref.Parse(s)
	if err != nil || r.Kind != ref.KindIndex || r.Version != "" {
		return "", fmt.Errorf("%q is not owner/repo", s)
	}
	return foldRepo(r.ID()), nil
}

func foldRepo(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
