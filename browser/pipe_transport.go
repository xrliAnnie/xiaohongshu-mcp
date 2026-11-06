package browser

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/go-rod/rod/lib/cdp"
)

const maxCDPPipeFrame = 32 * 1024 * 1024

var errCDPPipe = errors.New("cdp_pipe_unavailable")

type pipeTransport struct {
	onClose         func()
	reader          *bufio.Reader
	input           io.ReadCloser
	output          io.WriteCloser
	readMu, writeMu sync.Mutex
	once            sync.Once
	closed          atomic.Bool
}

var _ cdp.WebSocketable = (*pipeTransport)(nil)

func newPipeTransport(input io.ReadCloser, output io.WriteCloser) *pipeTransport {
	return &pipeTransport{reader: bufio.NewReaderSize(input, 65536), input: input, output: output}
}
func (p *pipeTransport) Send(data []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.closed.Load() {
		return errCDPPipe
	}
	if len(data) > maxCDPPipeFrame || !utf8.Valid(data) || !json.Valid(data) {
		p.Close()
		return errCDPPipe
	}
	for _, chunk := range [][]byte{data, {0}} {
		for len(chunk) > 0 {
			n, err := p.output.Write(chunk)
			if err != nil || n <= 0 || n > len(chunk) {
				p.Close()
				return errCDPPipe
			}
			chunk = chunk[n:]
		}
	}
	return nil
}
func (p *pipeTransport) Read() ([]byte, error) {
	p.readMu.Lock()
	defer p.readMu.Unlock()
	if p.closed.Load() {
		return nil, errCDPPipe
	}
	var frame []byte
	for {
		piece, err := p.reader.ReadSlice(0)
		if len(frame)+len(piece) > maxCDPPipeFrame+1 {
			p.Close()
			return nil, errCDPPipe
		}
		frame = append(frame, piece...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil || len(frame) == 0 || frame[len(frame)-1] != 0 {
			p.Close()
			return nil, errCDPPipe
		}
		frame = frame[:len(frame)-1]
		if len(frame) > maxCDPPipeFrame || !utf8.Valid(frame) || !json.Valid(frame) {
			p.Close()
			return nil, errCDPPipe
		}
		return frame, nil
	}
}
func (p *pipeTransport) Close() error {
	p.once.Do(func() {
		p.closed.Store(true)
		p.input.Close()
		p.output.Close()
		if p.onClose != nil {
			p.onClose()
		}
	})
	return nil
}
