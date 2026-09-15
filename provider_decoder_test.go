package main

import (
	"io"
	"strings"
	"testing"
)

func TestProviderDecoderRejectsInvalidStreamShape(t *testing.T) {
	good := []byte(`{"streams":[{"codec_type":"video","codec_name":"png","width":16,"height":16}]}`)
	if !validDecoderProbe(good, "image/png") {
		t.Fatal("valid image rejected")
	}
	for _, raw := range []string{
		`{"streams":[]}`,
		`{"streams":[{"codec_type":"audio"}]}`,
		`{"streams":[{"codec_type":"video","codec_name":"png","width":0,"height":16}]}`,
		`{"streams":[{"codec_type":"video","codec_name":"png","width":100000,"height":100000}]}`,
		`{"streams":[{"codec_type":"video","codec_name":"mjpeg","width":16,"height":16}]}`,
		`{"streams":[{"codec_type":"video","codec_name":"png","width":16,"height":16},{"codec_type":"audio"}]}`,
	} {
		if validDecoderProbe([]byte(raw), "image/png") {
			t.Fatal("accepted invalid streams", raw)
		}
	}
}

type decoderReaderOnly struct{ io.Reader }

func TestProviderDecoderOutputLimitAppliesToIOCopy(t *testing.T) {
	var output decoderOutput
	if _, err := io.Copy(&output, decoderReaderOnly{strings.NewReader(strings.Repeat("x", 65537))}); err == nil {
		t.Fatal("io.Copy bypassed output bound")
	}
}
