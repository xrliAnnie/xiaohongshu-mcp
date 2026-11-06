package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/cdp"
)

type testWriteCloser struct{ io.Writer }

func (testWriteCloser) Close() error { return nil }
func TestPipeTransportFramingAndFragmentedReads(t *testing.T) {
	var output bytes.Buffer
	transport := newPipeTransport(io.NopCloser(strings.NewReader("{\"id\":1}\x00{\"id\":2}\x00")), testWriteCloser{&output})
	defer transport.Close()
	for _, want := range []string{`{"id":1}`, `{"id":2}`} {
		got, err := transport.Read()
		if err != nil || string(got) != want {
			t.Fatalf("read=%q err=%v", got, err)
		}
	}
	if err := transport.Send([]byte(`{"id":3}`)); err != nil {
		t.Fatal(err)
	}
	if output.String() != "{\"id\":3}\x00" {
		t.Fatalf("framing=%q", output.String())
	}
}
func TestPipeTransportRejectsInvalidAndOversizedFrames(t *testing.T) {
	for _, input := range []string{"not-json\x00", "{}"} {
		transport := newPipeTransport(io.NopCloser(strings.NewReader(input)), testWriteCloser{io.Discard})
		if _, err := transport.Read(); err == nil {
			t.Fatal("accepted malformed/truncated frame")
		}
		if err := transport.Send([]byte(`{}`)); err == nil {
			t.Fatal("reopened closed transport")
		}
	}
	transport := newPipeTransport(io.NopCloser(strings.NewReader("")), testWriteCloser{io.Discard})
	defer transport.Close()
	if err := transport.Send(bytes.Repeat([]byte("x"), 32*1024*1024+1)); err == nil {
		t.Fatal("accepted oversized frame")
	}
}
func TestPipeTransportSerializesConcurrentSends(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	transport := newPipeTransport(io.NopCloser(strings.NewReader("")), writer)
	defer transport.Close()
	receiver := newPipeTransport(reader, testWriteCloser{io.Discard})
	defer receiver.Close()
	var senders sync.WaitGroup
	for i := 0; i < 20; i++ {
		senders.Add(1)
		go func(id int) {
			defer senders.Done()
			data, _ := json.Marshal(map[string]int{"id": id})
			if err := transport.Send(data); err != nil {
				t.Error(err)
			}
		}(i)
	}
	seen := map[int]bool{}
	for i := 0; i < 20; i++ {
		data, err := receiver.Read()
		if err != nil {
			t.Fatal(err)
		}
		var item struct {
			ID int `json:"id"`
		}
		if json.Unmarshal(data, &item) != nil || seen[item.ID] {
			t.Fatal("interleaved or repeated frame")
		}
		seen[item.ID] = true
	}
	senders.Wait()
}
func TestPipeTransportCloseUnblocksRead(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	transport := newPipeTransport(reader, testWriteCloser{io.Discard})
	done := make(chan error, 1)
	go func() { _, err := transport.Read(); done <- err }()
	transport.Close()
	transport.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read survived close")
		}
	case <-time.After(time.Second):
		t.Fatal("read stuck after close")
	}
}
func TestPipeTransportSupportsRodCDPWithoutWebSocket(t *testing.T) {
	commandsR, commandsW, _ := os.Pipe()
	responsesR, responsesW, _ := os.Pipe()
	transport := newPipeTransport(responsesR, commandsW)
	defer transport.Close()
	browser := newPipeTransport(commandsR, responsesW)
	defer browser.Close()
	done := make(chan error, 1)
	go func() {
		data, err := browser.Read()
		if err != nil {
			done <- err
			return
		}
		var request struct {
			ID int `json:"id"`
		}
		err = json.Unmarshal(data, &request)
		if err != nil {
			done <- err
			return
		}
		response, _ := json.Marshal(map[string]any{"id": request.ID, "result": map[string]string{"product": "fake"}})
		done <- browser.Send(response)
	}()
	client := cdp.New().Logger(log.New(io.Discard, "", 0)).Start(transport)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := client.Call(ctx, "", "Browser.getVersion", nil)
	if err != nil || string(result) != `{"product":"fake"}` {
		t.Fatalf("result=%s err=%v", result, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type pipeSpaces struct{}

func (pipeSpaces) Read(data []byte) (int, error) {
	for i := range data {
		data[i] = ' '
	}
	return len(data), nil
}
func TestPipeTransportBoundsIncomingFrame(t *testing.T) {
	for _, size := range []int{32 * 1024 * 1024, 32*1024*1024 + 1} {
		input := io.MultiReader(strings.NewReader("["), io.LimitReader(pipeSpaces{}, int64(size-2)), strings.NewReader("]\x00"))
		transport := newPipeTransport(io.NopCloser(input), testWriteCloser{io.Discard})
		data, err := transport.Read()
		transport.Close()
		if size == 32*1024*1024 {
			if err != nil || len(data) != size {
				t.Fatal("boundary frame rejected")
			}
		} else if err == nil {
			t.Fatal("oversized incoming frame accepted")
		}
	}
}
