// Package s3bucket is lib/bucket's Bucket on Amazon S3, with S3's own
// conditional requests: If-None-Match on GetObject, and If-Match or
// If-None-Match: * on PutObject (S3 conditional writes), which the
// publisher's compare-and-swap depends on (control-plane.md §4).
package s3bucket

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/chucklehead-dev/golem/lib/bucket"
)

// API is the part of the S3 client a Bucket uses.
type API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Bucket is an S3 bucket, its keys under Prefix.
type Bucket struct {
	Client API
	Name   string
	Prefix string // e.g. "golem/": prepended to every key
	// MaxObject bounds an object read (default 256 MiB): a snapshot or
	// pointer larger than this is refused, not buffered.
	MaxObject int64
}

// defaultMaxObject is MaxObject's default.
const defaultMaxObject = 256 << 20

var _ bucket.Bucket = (*Bucket)(nil)

func (b *Bucket) Get(ctx context.Context, key, ifNoneMatch string) ([]byte, string, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(b.Name), Key: aws.String(b.Prefix + key)}
	if ifNoneMatch != "" {
		in.IfNoneMatch = aws.String(ifNoneMatch)
	}
	out, err := b.Client.GetObject(ctx, in)
	if err != nil {
		return nil, "", mapErr(key, err)
	}
	defer out.Body.Close()
	limit := b.MaxObject
	if limit <= 0 {
		limit = defaultMaxObject
	}
	body, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("bucket: reading s3://%s/%s%s: %w", b.Name, b.Prefix, key, err)
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("bucket: s3://%s/%s%s is over %d bytes: refused", b.Name, b.Prefix, key, limit)
	}
	return body, aws.ToString(out.ETag), nil
}

func (b *Bucket) Put(ctx context.Context, key string, body []byte, ifMatch string) (string, error) {
	in := &s3.PutObjectInput{Bucket: aws.String(b.Name), Key: aws.String(b.Prefix + key),
		Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))}
	switch ifMatch {
	case "":
	case bucket.Absent:
		in.IfNoneMatch = aws.String("*")
	default:
		in.IfMatch = aws.String(ifMatch)
	}
	out, err := b.Client.PutObject(ctx, in)
	if err != nil {
		return "", mapErr(key, err)
	}
	return aws.ToString(out.ETag), nil
}

// mapErr turns S3's statuses into the bucket's errors: 404 (no such key),
// 304 (a conditional get, unchanged), 412 (a condition failed) and 409 (a
// conditional write racing another: S3's ConditionalRequestConflict, which
// is the same outcome for a compare-and-swap).
func mapErr(key string, err error) error {
	// A missing bucket is a 404 too, and must not read as a missing object:
	// the state store would take it for no state yet, and a gateway for a
	// pool never published.
	if ae, ok := errors.AsType[smithy.APIError](err); ok && ae.ErrorCode() == "NoSuchBucket" {
		return fmt.Errorf("bucket: %s: the bucket does not exist (check -bucket and the region): %w", key, err)
	}
	if re, ok := errors.AsType[*smithyhttp.ResponseError](err); ok {
		switch re.HTTPStatusCode() {
		case http.StatusNotFound:
			return bucket.ErrNotFound
		case http.StatusNotModified:
			return bucket.ErrNotModified
		case http.StatusPreconditionFailed, http.StatusConflict:
			return bucket.ErrPrecondition
		}
	}
	return fmt.Errorf("bucket: %s: %w", key, err)
}
