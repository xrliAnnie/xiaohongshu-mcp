package main

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
)

const privateQRPrefix = "data:image/png;base64,"
const privateQRMaxBytes = 384 * 1024

// Only this bounded PNG representation may cross the private login boundary.
// Re-encoding drops ancillary metadata; URLs are never fetched here.
func normalizePrivateLoginQR(value string) (string, error) {
	if !strings.HasPrefix(value, privateQRPrefix) || len(value) > len(privateQRPrefix)+base64.StdEncoding.EncodedLen(privateQRMaxBytes) {
		return "", errPrivateProvider
	}
	encoded := strings.TrimPrefix(value, privateQRPrefix)
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > privateQRMaxBytes || base64.StdEncoding.EncodeToString(raw) != encoded {
		return "", errPrivateProvider
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 2048 || config.Height > 2048 {
		return "", errPrivateProvider
	}
	reader := bytes.NewReader(raw)
	img, err := png.Decode(reader)
	if err != nil || reader.Len() != 0 {
		return "", errPrivateProvider
	}
	var normalized bytes.Buffer
	if png.Encode(&normalized, img) != nil || normalized.Len() > privateQRMaxBytes {
		return "", errPrivateProvider
	}
	return privateQRPrefix + base64.StdEncoding.EncodeToString(normalized.Bytes()), nil
}
