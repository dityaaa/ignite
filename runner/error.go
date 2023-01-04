package runner

import "errors"

var (
	// ErrBuildFailed is returned when a build fails.
	ErrBuildFailed = errors.New("build failed")

	// ErrBuildKilled is returned when a build is killed in the middle of building due
	// to a new change occurred.
	ErrBuildKilled = errors.New("build killed")
)
