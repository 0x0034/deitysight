package agent

import (
	"context"
	"errors"
	"os"
)

func sourceCode(e error) string {
	switch {
	case errors.Is(e, errTruncated):
		return "source_truncated"
	case errors.Is(e, os.ErrPermission):
		return "permission_denied"
	case errors.Is(e, os.ErrNotExist):
		return "source_unavailable"
	case errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled):
		return "sample_timeout"
	default:
		return "read_failed"
	}
}

var errTruncated = errors.New("source truncated")
