package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// discoverInputs expands the command-line inputs into log files. Files named
// directly are always returned (and fail later if they are not logs). Files
// found by walking a directory are sniffed first: anything that is not a
// MongoDB log (FTDC, archives, notes) is returned in skipped instead.
func discoverInputs(inputs []string) ([]string, []FileFailure, []string) {
	var files []string
	var failures []FileFailure
	var skipped []string

	for _, input := range inputs {
		info, err := os.Stat(input)
		if err != nil {
			failures = append(failures, FileFailure{Path: input, Reason: err.Error()})
			continue
		}

		if info.Mode().IsRegular() {
			files = append(files, input)
			continue
		}

		if !info.IsDir() {
			failures = append(failures, FileFailure{Path: input, Reason: "not a regular file or directory"})
			continue
		}

		err = filepath.WalkDir(input, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				failures = append(failures, FileFailure{Path: path, Reason: walkErr.Error()})
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			format, err := sniffLogFile(path)
			if err != nil {
				failures = append(failures, FileFailure{Path: path, Reason: err.Error()})
				return nil
			}
			if format == formatUnknown {
				skipped = append(skipped, path)
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrPermission) {
			failures = append(failures, FileFailure{Path: input, Reason: err.Error()})
		}
	}

	sort.Strings(files)
	sort.Strings(skipped)
	return files, failures, skipped
}
