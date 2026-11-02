package opendesign

import (
	"bytes"
)

// sseDecoder turns an arbitrarily chunked text/event-stream byte stream into
// complete event payloads. Frames may be split across reads or bundled together,
// so the decoder owns its own buffer.
type sseDecoder struct {
	buf      []byte
	maxBytes int
}

// sseEvent is one dispatched event. Data is the concatenated `data:` payload
// with no trailing newline, matching the OpenAI/CPA wire contract.
type sseEvent struct {
	Event string
	Data  []byte
}

// feed appends chunk to the buffer and dispatches every complete event. The
// callback may return an error to abort the stream.
func (d *sseDecoder) feed(chunk []byte, dispatch func(sseEvent) error) error {
	d.buf = append(d.buf, chunk...)
	if d.maxBytes > 0 && len(d.buf) > d.maxBytes {
		return upstreamProtocolError("upstream SSE buffer exceeds configured limit")
	}
	// Normalize CRLF so a frame split between "\r" and "\n" still assembles.
	// A trailing lone "\r" stays pending until the next chunk reveals its pair.
	if bytes.IndexByte(d.buf, '\r') >= 0 {
		d.buf = bytes.ReplaceAll(d.buf, []byte("\r\n"), []byte("\n"))
	}
	for {
		index := bytes.Index(d.buf, []byte("\n\n"))
		if index < 0 {
			return nil
		}
		// Parse before shifting the buffer: the remaining bytes are copied over
		// the same array, which would corrupt a block held as a slice.
		block := d.buf[:index]
		event, ok := parseEventBlock(block)
		rest := d.buf[index+2:]
		remaining := len(rest)
		d.buf = append(d.buf[:0], rest...)
		d.buf = d.buf[:remaining]
		if !ok {
			continue
		}
		if err := dispatch(event); err != nil {
			return err
		}
	}
}

// end reports a truncated trailing frame. A clean EOF after a complete event
// leaves buf empty and returns nil.
func (d *sseDecoder) end() error {
	if len(bytes.TrimSpace(d.buf)) == 0 {
		return nil
	}
	return upstreamProtocolError("upstream stream ended with an incomplete SSE frame")
}

func parseEventBlock(block []byte) (sseEvent, bool) {
	block = bytes.ReplaceAll(block, []byte("\r\n"), []byte("\n"))
	var event sseEvent
	var data []byte
	found := false
	for _, rawLine := range bytes.Split(block, []byte("\n")) {
		line := bytes.TrimSuffix(rawLine, []byte("\r"))
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		name, value, hasValue := bytes.Cut(line, []byte(":"))
		if hasValue && len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(name) {
		case "event":
			event.Event = string(value)
		case "data":
			if found {
				data = append(data, '\n')
			}
			data = append(data, value...)
			found = true
		}
	}
	if !found {
		return sseEvent{}, false
	}
	event.Data = data
	return event, true
}
