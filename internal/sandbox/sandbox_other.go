//go:build !linux || (!amd64 && !arm64)

package sandbox

import "errors"

func RestrictProcessControl() error { return errors.New("sandbox requires Linux amd64 or arm64") }
