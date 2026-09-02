package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
)

// Temporary probe: dump the raw error body from rustfs for a signed PUT.
func TestProbeS3Put403(t *testing.T) {
	endpoint := os.Getenv("WALHUB_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("no endpoint")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "walgit-dev")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "walgit-dev-secret")
	s, err := NewS3(&config.Store{
		Bucket:  "walhub-test",
		Backend: "s3",
		S3:      config.S3{Endpoint: endpoint, ForcePathStyle: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.Put(context.Background(), "probe/x.txt", PutBody{Bytes: []byte("hello")}, PutOptions{Mode: PutCreate})
	fmt.Printf("PUT meta=%+v err=%v\n", meta, err)
	if err != nil {
		fmt.Printf("put error type=%T\n", err)
		// raw signed request to dump the XML body
		req, rerr := s.signedReq(context.Background(), "PUT", "probe/raw.txt", nil, emptyHash, strings.NewReader("raw"), 3)
		if rerr != nil {
			t.Fatal(rerr)
		}
		res, rerr := s.control.Do(req)
		if rerr != nil {
			t.Fatal(rerr)
		}
		defer res.Body.Close()
		buf := make([]byte, 900)
		n, _ := res.Body.Read(buf)
		fmt.Printf("RAW PUT status=%d body=%s\n", res.StatusCode, string(buf[:n]))
	}
}
