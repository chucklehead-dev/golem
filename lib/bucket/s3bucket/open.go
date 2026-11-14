package s3bucket

import (
	"context"
	"errors"
	"strings"

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
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &Bucket{Client: s3.NewFromConfig(awsCfg), Name: name, Prefix: prefix}, nil
}
