//go:build (!linux && !darwin) || baremetal

package os

import "errors"

func Executable() (string, error) {
	return "", errors.New("Executable not implemented")
}
