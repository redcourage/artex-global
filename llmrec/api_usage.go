package llmrec

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/llm"
)

const maxAPIUsageBytes = 1024 * 1024

// apiUsageSnapshot contains counters only. Optional fields distinguish a
// measured zero from a response with no reported usage.
type apiUsageSnapshot struct {
	Reported           bool
	Incomplete         bool
	Input              *int
	Output             *int
	CacheRead          *int
	InputExcludesCache bool
	CacheWrite         *int
	Reasoning          *int
}

type apiUsageObserver struct {
	mu         sync.Mutex
	state      apiUsageSnapshot
	streaming  bool
	buffer     []byte
	data       []byte
	frameBytes int
	discard    bool
	finished   bool
}

// ObserveResponse inspects usage fields as the SDK reads a successful response.
// JSON is bounded to 1 MiB and SSE to 1 MiB per event; buffers are transient and
// never persisted. Each successful HTTP attempt replaces the prior observer so
// SDK retries do not double count. Bridge headers remain authoritative.
func (m *Metering) ObserveResponse(resp *http.Response) io.ReadCloser {
	if resp == nil {
		return nil
	}
	if m == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Body
	}
	observer := &apiUsageObserver{streaming: strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")}
	m.mu.Lock()
	m.api = observer
	m.mu.Unlock()
	return &apiUsageBody{ReadCloser: resp.Body, observer: observer}
}

type apiUsageBody struct {
	io.ReadCloser
	observer *apiUsageObserver
}

func (b *apiUsageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.observer.feed(p[:n], err == io.EOF)
	return n, err
}
func (b *apiUsageBody) Close() error {
	err := b.ReadCloser.Close()
	b.observer.feed(nil, true)
	return err
}

func (o *apiUsageObserver) feed(chunk []byte, final bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	if !o.streaming {
		if !o.discard {
			if len(o.buffer)+len(chunk) > maxAPIUsageBytes {
				o.state.Incomplete = true
				o.discard = true
				o.buffer = nil
			} else {
				o.buffer = append(o.buffer, chunk...)
			}
		}
		if final && !o.discard {
			o.parse(o.buffer)
			o.buffer = nil
		}
	} else {
		for _, ch := range chunk {
			if ch == '\n' {
				line := bytes.TrimSuffix(o.buffer, []byte("\r"))
				if len(line) == 0 && !o.discard {
					o.flushEvent()
				} else if len(line) == 0 {
					o.discard = false
					o.frameBytes = 0
				} else if !o.discard && bytes.HasPrefix(line, []byte("data:")) {
					data := bytes.TrimPrefix(line[5:], []byte(" "))
					o.data = append(o.data, data...)
					o.data = append(o.data, '\n')
				}
				o.buffer = o.buffer[:0]
				continue
			}
			if !o.discard {
				o.frameBytes++
			}
			if !o.discard && o.frameBytes > maxAPIUsageBytes {
				o.state.Incomplete = true
				o.discard = true
				o.buffer = nil
				o.data = nil
			}
			// While discarding keep at most one marker byte so a nonempty line is not
			// mistaken for an event boundary; the next blank line resumes parsing.
			if o.discard {
				if len(o.buffer) == 0 && ch != '\r' {
					o.buffer = append(o.buffer, 'x')
				}
			} else {
				o.buffer = append(o.buffer, ch)
			}
		}
		if final && !o.discard {
			if bytes.HasPrefix(o.buffer, []byte("data:")) {
				o.data = append(o.data, bytes.TrimSpace(o.buffer[5:])...)
			}
			o.flushEvent()
			o.buffer = nil
		}
	}
	if final {
		o.finished = true
	}
}

func (o *apiUsageObserver) flushEvent() {
	data := bytes.TrimSpace(o.data)
	if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
		o.parse(data)
	}
	o.data = nil
	o.frameBytes = 0
}

func (o *apiUsageObserver) parse(data []byte) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		o.state.Incomplete = true
		return
	}
	// Chat Completions and Anthropic delta use usage at the root; Responses
	// completion and Anthropic message_start put it on response/message.
	usage := root["usage"]
	if len(usage) == 0 {
		for _, key := range []string{"response", "message"} {
			var nested map[string]json.RawMessage
			if json.Unmarshal(root[key], &nested) == nil && len(nested["usage"]) > 0 {
				usage = nested["usage"]
				break
			}
		}
	}
	if len(usage) == 0 || bytes.Equal(bytes.TrimSpace(usage), []byte("null")) {
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(usage, &fields) != nil {
		o.state.Incomplete = true
		return
	}
	read := func(raw json.RawMessage) *int {
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		var n int
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 1<<31-1 {
			o.state.Incomplete = true
			return nil
		}
		return &n
	}
	set := func(target **int, raw json.RawMessage) {
		if n := read(raw); n != nil {
			*target = n
		}
	}
	for _, key := range []string{"prompt_tokens", "input_tokens"} {
		set(&o.state.Input, fields[key])
	}
	for _, key := range []string{"completion_tokens", "output_tokens"} {
		set(&o.state.Output, fields[key])
	}
	for _, key := range []string{"cache_read_tokens", "cache_read_input_tokens"} {
		set(&o.state.CacheRead, fields[key])
	}
	// Anthropic input_tokens excludes cache reads/writes; SDK input includes them.
	if _, ok := fields["cache_read_input_tokens"]; ok {
		o.state.InputExcludesCache = true
	}
	if _, ok := fields["cache_creation_input_tokens"]; ok {
		o.state.InputExcludesCache = true
	}
	o.state.Reported = o.state.Input != nil && o.state.Output != nil
	for _, key := range []string{"cache_write_tokens", "cache_creation_input_tokens"} {
		set(&o.state.CacheWrite, fields[key])
	}
	set(&o.state.Reasoning, fields["reasoning_tokens"])
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details", "completion_tokens_details", "output_tokens_details"} {
		var details map[string]json.RawMessage
		if raw := fields[key]; len(raw) > 0 && json.Unmarshal(raw, &details) != nil {
			o.state.Incomplete = true
			continue
		}
		set(&o.state.CacheRead, details["cached_tokens"])
		for _, name := range []string{"cache_write_tokens", "cache_creation_tokens"} {
			set(&o.state.CacheWrite, details[name])
		}
		set(&o.state.Reasoning, details["reasoning_tokens"])
	}
}

func (o *apiUsageObserver) snapshot() apiUsageSnapshot {
	if o == nil {
		return apiUsageSnapshot{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state
}

func (s meteringSnapshot) applyAPI(usage llm.Usage) llm.Usage {
	if s.Present || s.API.Incomplete {
		return usage
	}
	if s.API.CacheWrite != nil {
		usage.CacheWriteTokens = *s.API.CacheWrite
	}
	return usage
}

// knownUsage requires both main counters before treating SDK zero as a measured
// zero. A body reader may observe later usage before the SDK delivers those
// events; if observed totals exceed delivered totals, preserve unknown status
// instead of assigning a zero or partial cost to that interrupted call.
func (s apiUsageSnapshot) knownUsage(usage llm.Usage) bool {
	if s.Incomplete {
		return false
	}
	positive := usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheReadTokens > 0 || usage.CacheWriteTokens > 0
	if s.Input != nil {
		input := int64(*s.Input)
		if s.InputExcludesCache {
			if s.CacheRead != nil {
				input += int64(*s.CacheRead)
			}
			if s.CacheWrite != nil {
				input += int64(*s.CacheWrite)
			}
		}
		if input > int64(usage.InputTokens) {
			return false
		}
	}
	if s.Output != nil && *s.Output > usage.OutputTokens {
		return false
	}
	if s.CacheRead != nil && *s.CacheRead > usage.CacheReadTokens {
		return false
	}
	if s.Reasoning != nil && *s.Reasoning > usage.OutputTokens {
		return false
	}
	return positive || s.Reported
}
