package main

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/vincentsch/rungrad/output"
)

// rawJQWriter prints --jq results that are strings without JSON quotes, one
// per line, the way gh does. Other results (numbers, objects, arrays) stay
// JSON. rungrad writes all --jq output in one call, one result per line.
type rawJQWriter struct {
	w io.Writer
}

func (r *rawJQWriter) Write(p []byte) (int, error) {
	lines := bytes.SplitAfter(p, []byte("\n"))
	var out bytes.Buffer
	for _, line := range lines {
		trimmed := bytes.TrimSuffix(line, []byte("\n"))
		if len(trimmed) > 1 && trimmed[0] == '"' {
			var s string
			if json.Unmarshal(trimmed, &s) == nil {
				// Decoding turns \u001b back into a real escape byte, so
				// strip terminal controls again.
				out.Write(output.SanitizeControlBytes([]byte(s)))
				if len(trimmed) < len(line) {
					out.WriteByte('\n')
				}
				continue
			}
		}
		out.Write(line)
	}
	if _, err := r.w.Write(out.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}
