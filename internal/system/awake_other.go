//go:build !darwin || !cgo

package system

import "errors"

func holdSleepAssertion(string) (func() error, error) {
	return nil, errors.New("power assertions require macOS with cgo enabled")
}
