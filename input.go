package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

func discoverInputs(inputs []string) ([]string, []FileFailure) {
	var files []string
	var failures []FileFailure

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
			if d.Type().IsRegular() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrPermission) {
			failures = append(failures, FileFailure{Path: input, Reason: err.Error()})
		}
	}

	sort.Strings(files)
	return files, failures
}
