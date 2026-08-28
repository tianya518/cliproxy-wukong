package grok

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func TestDecodeWireBodyGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`{"user":{"id":"497f19f8-49d4-458a-bee4-43ec3dcaf8ca"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := decodeWireBody(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	id, err := parseSession(got)
	if err != nil || id.UserID == "" {
		t.Fatalf("%q %v %#v", got, err, id)
	}
}

func TestDecodeWireBodyGzipKeepsLargeHTML(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 80<<10)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := decodeWireBody(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(payload))
	}
}
