package fetch

import (
	"bytes"
	"compress/gzip"
	"io"
)

// maybeDecompress 在响应是 gzip 编码时解压。
//
// 这里手动设置了 Accept-Encoding: gzip，而 Go 的 Transport 只有在调用方
// 未设置该头时才会自动解压，所以需要自行处理。
func maybeDecompress(body []byte, encoding string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	// 只处理 gzip，其余编码（br / deflate）交给上层报错。
	if !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		return body, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, maxBodyBytes))
}
