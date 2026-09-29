package servercmd

import (
	"errors"
	"io"
)

func install(_ []string, _, _ io.Writer) error {
	return errors.New("install: not implemented yet")
}

func uninstall(_ []string, _, _ io.Writer) error {
	return errors.New("uninstall: not implemented yet")
}
