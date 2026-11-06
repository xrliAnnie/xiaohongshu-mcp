package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func qrFixture(t *testing.T, width int) string {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, width, width))
	img.SetGray(0, 0, color.Gray{Y: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(out.Bytes())
}
func TestPrivateLoginQRAcceptsOnlyBoundedDecodedPNG(t *testing.T) {
	valid := qrFixture(t, 32)
	result, err := normalizePrivateLoginQR(valid)
	if err != nil || !strings.HasPrefix(result, "data:image/png;base64,") {
		t.Fatal("valid QR rejected")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(result, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil || decoded.Bounds().Dx() != 32 || decoded.Bounds().Dy() != 32 || color.GrayModel.Convert(decoded.At(0, 0)).(color.Gray).Y != 255 || color.GrayModel.Convert(decoded.At(1, 1)).(color.Gray).Y != 0 {
		t.Fatal("QR pixels changed")
	}
	for _, value := range []string{"https://example.test/qr.png", "data:image/svg+xml;base64,PHN2Zy8+", "data:image/png;base64,bm90LXBuZw==", valid + "\n", qrFixture(t, 2049), "data:image/png;base64," + strings.Repeat("A", 700000)} {
		if _, err := normalizePrivateLoginQR(value); err == nil {
			t.Fatal("unsafe QR accepted")
		}
	}
}
func TestPrivateLoginQRRejectsTrailingPrivateBytes(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(qrFixture(t, 32), "data:image/png;base64,"))
	raw = append(raw, []byte("PRIVATE_TOKEN_CANARY")...)
	if _, err := normalizePrivateLoginQR("data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("trailing private bytes accepted")
	}
}

func TestPrivateLoginQRRemovesAncillaryMetadata(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(qrFixture(t, 32), privateQRPrefix))
	payload := []byte("Comment\x00PRIVATE_METADATA_CANARY")
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], "tEXt")
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	joined := append(append(append([]byte{}, raw[:len(raw)-12]...), chunk...), raw[len(raw)-12:]...)
	result, err := normalizePrivateLoginQR(privateQRPrefix + base64.StdEncoding.EncodeToString(joined))
	if err != nil {
		t.Fatal("valid metadata PNG rejected")
	}
	normalized, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(result, privateQRPrefix))
	if bytes.Contains(normalized, []byte("PRIVATE_METADATA_CANARY")) {
		t.Fatal("metadata crossed QR boundary")
	}
}
