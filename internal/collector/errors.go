package collector

import (
	"context"
	"errors"
	"net"

	"github.com/dsmithson/EngineeringActivity/internal/gitmirror"
	"github.com/dsmithson/EngineeringActivity/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/microsoft/azure-devops-go-api/azuredevops/v7"
)

// Classify maps an error to a small, stable set of classes safe for logs and metric
// labels. Raw error messages are never used as labels.
func Classify(err error) string {
	if err == nil {
		return ""
	}
	var wrapped azuredevops.WrappedError
	var wrappedPtr *azuredevops.WrappedError
	status := 0
	if errors.As(err, &wrapped) && wrapped.StatusCode != nil {
		status = *wrapped.StatusCode
	} else if errors.As(err, &wrappedPtr) && wrappedPtr.StatusCode != nil {
		status = *wrappedPtr.StatusCode
	}
	var gitErr *gitmirror.ErrGit
	var pgErr *pgconn.PgError
	var netErr net.Error
	var cfg configError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, store.ErrLocked):
		return "locked"
	case errors.As(err, &cfg):
		return "config"
	case status == 401 || status == 403:
		return "auth"
	case status == 404:
		return "not_found"
	case status == 429:
		return "throttled"
	case status >= 500:
		return "upstream_5xx"
	case status >= 400:
		return "upstream_4xx"
	case errors.As(err, &gitErr):
		return "git"
	case errors.As(err, &pgErr):
		return "database"
	case errors.As(err, &netErr):
		return "network"
	default:
		return "other"
	}
}

type configError struct{ msg string }

func (e configError) Error() string { return e.msg }
