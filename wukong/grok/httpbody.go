package grok

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

func decodeWireBody(raw []byte) ([]byte, error) {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("解压 Grok gzip 响应: %w", err)
		}
		defer zr.Close()
		out, err := io.ReadAll(io.LimitReader(zr, responseBodyLimit+1))
		if err != nil {
			return nil, fmt.Errorf("读取 Grok gzip 响应: %w", err)
		}
		if int64(len(out)) > responseBodyLimit {
			return nil, fmt.Errorf("Grok 响应超过安全上限")
		}
		return out, nil
	}
	return raw, nil
}
