package upstream

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
)

var errStreamDone = errors.New("conversation stream complete")

type Event struct {
	Name string
	Data string
}

// ReadSSE handles fragmented transport reads, CRLF, comments and multiline data.
// The owning HTTP request must carry ctx to interrupt a blocked network read.
func ReadSSE(ctx context.Context, r io.Reader, maxEvent int, maxStream int64, consume func(Event) error) error {
	if maxEvent <= 0 || maxEvent > 96<<20 || maxStream <= 0 || maxStream > 256<<20 || r == nil || consume == nil {
		return failure("invalid_limits", "sse")
	}
	limited := &io.LimitedReader{R: r, N: maxStream + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, min(4096, maxEvent+1)), maxEvent+1)
	var data []string
	name := ""
	size := 0
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		err := consume(Event{Name: name, Data: strings.Join(data, "\n")})
		data = nil
		name = ""
		size = 0
		return err
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if limited.N == 0 {
			return failure("stream_too_large", "sse")
		}
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				if errors.Is(err, errStreamDone) {
					return nil
				}
				return err
			}
			name = ""
			size = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			size += len(value) + 1
			if size > maxEvent {
				return failure("event_too_large", "sse")
			}
			data = append(data, value)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if limited.N == 0 {
		return failure("stream_too_large", "sse")
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return failure("event_too_large", "sse")
		}
		return failure("stream_read_error", "sse")
	}
	// An unterminated final SSE event is not committed; callers require a
	// structured terminal marker or [DONE] before accepting a successful result.
	return nil
}
