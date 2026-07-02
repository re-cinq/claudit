package storage

import (
	"strings"
	"testing"
)

func TestCompressDecompressRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"simple text", []byte("Hello, world!")},
		{"empty data", []byte{}},
		{"JSONL transcript", []byte(`{"uuid":"1","type":"user"}` + "\n" + `{"uuid":"2","type":"assistant"}`)},
		{"binary-like data", []byte{0x00, 0xFF, 0x80, 0x7F, 0x01}},
		{"large data", []byte(strings.Repeat("abcdefghijklmnop", 10000))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compressed, err := Compress(tc.data)
			if err != nil {
				t.Fatalf("Compress() error: %v", err)
			}

			decompressed, err := Decompress(compressed)
			if err != nil {
				t.Fatalf("Decompress() error: %v", err)
			}

			if string(decompressed) != string(tc.data) {
				t.Errorf("round-trip failed: got %d bytes, want %d bytes", len(decompressed), len(tc.data))
			}
		})
	}
}

func TestDecompressInvalidData(t *testing.T) {
	_, err := Decompress([]byte("not gzip data"))
	if err == nil {
		t.Error("Decompress() should fail on invalid gzip data")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	data := []byte("Hello, world! Special chars: \x00\xFF\x80")
	encoded := Encode(data)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	if string(decoded) != string(data) {
		t.Error("Encode/Decode round-trip failed")
	}
}

func TestDecodeInvalidBase64(t *testing.T) {
	_, err := Decode("not valid base64!!!")
	if err == nil {
		t.Error("Decode() should fail on invalid base64")
	}
}

func TestChecksum(t *testing.T) {
	data := []byte("test data")
	checksum := Checksum(data)

	if !strings.HasPrefix(checksum, "sha256:") {
		t.Errorf("Checksum() = %q, want sha256: prefix", checksum)
	}

	// Same data should produce same checksum
	if Checksum(data) != checksum {
		t.Error("Checksum() not deterministic")
	}

	// Different data should produce different checksum
	if Checksum([]byte("different data")) == checksum {
		t.Error("Checksum() produced same hash for different data")
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("test data")
	checksum := Checksum(data)

	if !VerifyChecksum(data, checksum) {
		t.Error("VerifyChecksum() returned false for matching data")
	}

	if VerifyChecksum([]byte("tampered data"), checksum) {
		t.Error("VerifyChecksum() returned true for mismatched data")
	}

	if VerifyChecksum(data, "sha256:0000000000000000000000000000000000000000000000000000000000000000") {
		t.Error("VerifyChecksum() returned true for wrong checksum")
	}
}

func TestCompressAndEncodeDecodeAndDecompress(t *testing.T) {
	original := []byte(`{"uuid":"1","type":"user","message":{"content":[{"type":"text","text":"Hello"}]}}`)

	encoded, err := CompressAndEncode(original)
	if err != nil {
		t.Fatalf("CompressAndEncode() error: %v", err)
	}

	decoded, err := DecodeAndDecompress(encoded)
	if err != nil {
		t.Fatalf("DecodeAndDecompress() error: %v", err)
	}

	if string(decoded) != string(original) {
		t.Error("CompressAndEncode/DecodeAndDecompress round-trip failed")
	}
}

func TestDecodeAndDecompressInvalidBase64(t *testing.T) {
	_, err := DecodeAndDecompress("not valid base64!!!")
	if err == nil {
		t.Error("DecodeAndDecompress() should fail on invalid base64")
	}
}

func TestDecodeAndDecompressValidBase64InvalidGzip(t *testing.T) {
	// Valid base64 but not gzip data
	encoded := Encode([]byte("not gzip"))
	_, err := DecodeAndDecompress(encoded)
	if err == nil {
		t.Error("DecodeAndDecompress() should fail when base64 decodes to non-gzip data")
	}
}
