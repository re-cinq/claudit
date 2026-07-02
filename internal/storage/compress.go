package storage

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// Compress compresses data using gzip
func Compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)

	if _, err := w.Write(data); err != nil {
		return nil, err
	}

	if err := w.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Decompress decompresses gzip data
func Decompress(data []byte) (result []byte, err error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := r.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	return io.ReadAll(r)
}

// Encode encodes data as base64
func Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// Decode decodes base64 data
func Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// Checksum computes SHA256 checksum of data and returns it in "sha256:<hex>" format
func Checksum(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h)
}

// VerifyChecksum verifies that the data matches the expected checksum
func VerifyChecksum(data []byte, expected string) bool {
	return Checksum(data) == expected
}

// CompressAndEncode compresses and base64 encodes data
func CompressAndEncode(data []byte) (string, error) {
	compressed, err := Compress(data)
	if err != nil {
		return "", err
	}
	return Encode(compressed), nil
}

// DecodeAndDecompress decodes base64 and decompresses data
func DecodeAndDecompress(s string) ([]byte, error) {
	decoded, err := Decode(s)
	if err != nil {
		return nil, err
	}
	return Decompress(decoded)
}
