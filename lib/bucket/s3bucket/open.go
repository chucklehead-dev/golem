package s3bucket

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/chucklehead-dev/golem/lib/bucket"
)

// Open is the bucket where names: s3://<bucket>/<prefix>, with the default
// AWS credential chain (workload identity in the cluster), or else a
// directory (bucket.Dir: tests and development).
func Open(ctx context.Context, where string) (bucket.Bucket, error) {
	rest, ok := strings.CutPrefix(where, "s3://")
	if !ok {
		return bucket.OpenDir(where)
	}
	name, prefix, _ := strings.Cut(rest, "/")
	if name == "" {
		return nil, errors.New("s3://<bucket>/<prefix>: no bucket")
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithHTTPClient(httpClient(responseHeaderTimeout)))
	if err != nil {
		return nil, err
	}
	return &Bucket{Client: s3.NewFromConfig(awsCfg), Name: name, Prefix: prefix}, nil
}

// responseHeaderTimeout is how long the S3 client waits for a response's
// headers once its request is sent. The SDK's default client has no such
// bound: a server that accepts a request and never answers would hold a
// poll, a state write or a publication for good. A body is read under the
// caller's own deadline.
const responseHeaderTimeout = 15 * time.Second

// httpClient is the SDK's buildable client with a response-header timeout.
func httpClient(headers time.Duration) *awshttp.BuildableClient {
	return awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.ResponseHeaderTimeout = headers
	})
}
