// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.17 && !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/vulndb/internal/cve5"
	"golang.org/x/vulndb/internal/osvutils"
	"golang.org/x/vulndb/internal/proxy"
	"golang.org/x/vulndb/internal/report"
	"golang.org/x/vulndb/internal/triage/priority"
)

func TestChecksBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	// In short mode (used by presubmit checks), only do offline checks.
	var cmd *exec.Cmd
	if testing.Short() {
		cmd = exec.Command(bash, "./checks.bash", "offline")
	} else {
		cmd = exec.Command(bash, "./checks.bash")
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}
func TestLintReports(t *testing.T) {
	if runtime.GOOS == "android" {
		t.Skipf("android builder does not have access to reports/")
	}
	allFiles := make(map[string]string)
	var reports []string
	for _, dir := range []string{report.YAMLDir, report.ExcludedDir} {
		files, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unable to read %v/: %s", dir, err)
		}
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			if filepath.Ext(file.Name()) != ".yaml" {
				continue
			}
			filename := filepath.Join(dir, file.Name())
			if allFiles[file.Name()] != "" {
				t.Errorf("report appears in multiple locations: %v, %v", allFiles[file.Name()], filename)
			}
			allFiles[file.Name()] = filename
			reports = append(reports, filename)
		}
	}

	// Skip network calls in short mode.
	var lint func(r *report.Report) []string
	if testing.Short() {
		lint = func(r *report.Report) []string {
			return r.LintOffline()
		}
	} else {
		pc := proxy.NewDefaultClient()
		lint = func(r *report.Report) []string {
			return r.Lint(pc)
		}
	}

	ctx := context.Background()
	rc, err := report.NewLocalClient(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	modulesToImports, err := priority.LoadModuleMap()
	if err != nil {
		t.Fatal(err)
	}

	// Map from summaries to report paths, used to check for duplicate summaries.
	summaries := make(map[string]string)
	sort.Strings(reports)
	for _, filename := range reports {
		t.Run(filename, func(t *testing.T) {
			r, err := report.Read(filename)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.CheckFilename(filename); err != nil {
				t.Error(err)
			}
			lints := lint(r)
			if len(lints) > 0 {
				t.Error(strings.Join(lints, "\n"))
			}
			duplicates := make(map[string][]string)
			for _, alias := range r.Aliases() {
				for _, r2 := range rc.ReportsByAlias(alias) {
					if r2.ID != r.ID {
						duplicates[r2.ID] = append(duplicates[r2.ID], alias)
					}
				}
			}
			for r2, aliases := range duplicates {
				t.Errorf("report %s shares duplicate alias(es) %s with report %s", filename, aliases, r2)
			}
			// Ensure that each reviewed report has a unique summary.
			if r.IsReviewed() {
				if summary := r.Summary.String(); summary != "" {
					if report, ok := summaries[summary]; ok {
						t.Errorf("report %s shares duplicate summary %q with report %s", filename, summary, report)
					} else {
						summaries[summary] = filename
					}
				}
			}
			// Ensure that no unreviewed reports are high priority.
			// This can happen because the initial quick triage algorithm
			// doesn't know about all affected modules - just the one
			// listed in the Github issue.
			if r.IsUnreviewed() && !r.IsExcluded() && !r.UnreviewedOK {
				pr, _ := priority.AnalyzeReport(r, rc, modulesToImports)
				if pr.Priority == priority.High {
					t.Errorf("UNREVIEWED report %s is high priority (should be NEEDS_REVIEW or REVIEWED) - reason: %s", filename, pr.Reason)
				}
			}
			// Check that a correct OSV file was generated for each YAML report.
			if r.Excluded == "" {
				generated, err := r.ToOSV(time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				osvFilename := r.OSVFilename()
				current, err := report.ReadOSV(osvFilename)
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(generated, current, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("%s does not match report:\n%v", osvFilename, diff)
				}
				if err := osvutils.ValidateExceptTimestamps(&current); err != nil {
					t.Error(err)
				}
			}
			if r.CVEMetadata != nil {
				generated, err := cve5.FromReport(r)
				if err != nil {
					t.Fatal(err)
				}
				cvePath := r.CVEFilename()
				current, err := cve5.Read(cvePath)
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(generated, current, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("%s does not match report:\n%v", cvePath, diff)
				}

			}
		})
	}
}
