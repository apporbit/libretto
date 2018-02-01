// Copyright 2015 Apcera Inc. All rights reserved.

package vmrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func copyDir(src string, dest string) error {
	srcDir, err := os.Stat(src)
	if err != nil {
		return err
	}

	err = os.MkdirAll(dest, srcDir.Mode())
	if err != nil {
		return err
	}

	dir, err := os.Open(src)
	if err != nil {
		return err
	}
	defer dir.Close()

	// Pass -1 to Readdir to make it read everything into a single slice
	children, err := dir.Readdir(-1)
	if err != nil {
		return fmt.Errorf("failed to read files from directory %q: %v", src, err)
	}

	for _, child := range children {
		newSrc := filepath.Join(src, child.Name())
		newDst := filepath.Join(dest, child.Name())

		if child.IsDir() {
			err = copyDir(newSrc, newDst)
			if err != nil {
				return err
			}
		} else {
			err = copyFile(newSrc, newDst)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src string, dest string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	destfile, err := os.Create(dest)
	if err != nil {
		return err
	}

	defer destfile.Close()

	_, err = io.Copy(destfile, srcFile)
	if err == nil {
		srcinfo, err := os.Stat(src)
		if err != nil {
			err = os.Chmod(dest, srcinfo.Mode())
			if err != nil {
				return err
			}
		} else {
			return err
		}

	} else {
		return err
	}
	return nil
}
