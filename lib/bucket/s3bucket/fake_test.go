package s3bucket

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
)

// fakeS3 is S3's object API as far as Bucket uses it, path-style: GET and
// PUT of /<bucket>/<key>, quoted MD5 entity tags, If-None-Match on GET
// (304), If-Match and If-None-Match: * on PUT (412), 404 NoSuchKey, as S3
// documents them. Conditions are checked and applied under one lock, as
// S3 applies a conditional write atomically.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func etagOf(b []byte) string { h := md5.Sum(b); return `"` + hex.EncodeToString(h[:]) + `"` }

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if status != http.StatusNotModified {
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+code+`</Code><Message>`+code+`</Message></Error>`)
	}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, exists := f.objects[key]
	switch r.Method {
	case http.MethodGet:
		if !exists {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		tag := etagOf(cur)
		w.Header().Set("ETag", tag)
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == tag {
			s3Error(w, http.StatusNotModified, "NotModified")
			return
		}
		w.Write(cur)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if im := r.Header.Get("If-Match"); im != "" {
			if !exists {
				s3Error(w, http.StatusNotFound, "NoSuchKey")
				return
			}
			if im != etagOf(cur) {
				s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
				return
			}
		}
		if r.Header.Get("If-None-Match") == "*" && exists {
			s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		f.objects[key] = body
		w.Header().Set("ETag", etagOf(body))
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}
