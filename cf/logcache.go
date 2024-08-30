package cf

import (
	"context"
	"time"

	logcache "code.cloudfoundry.org/go-log-cache/v3"
	"code.cloudfoundry.org/go-loggregator/v10/rpc/loggregator_v2"
)

//go:generate counterfeiter -o mocks/logcache.go . LogCacheClient
type LogCacheClient interface {
	Read(
		ctx context.Context,
		sourceID string,
		start time.Time,
		opts ...logcache.ReadOption,
	) ([]*loggregator_v2.Envelope, error)
}
