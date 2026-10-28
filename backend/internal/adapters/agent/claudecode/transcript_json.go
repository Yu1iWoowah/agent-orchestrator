package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
)

// encoding/json accepts duplicate object keys and unpaired UTF-16 escapes.
// Neither is acceptable for provenance or a lossless text export.
func strictTranscriptJSON(raw []byte) error {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			break
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "incomplete Unicode escape")
		}
		v, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return e
		}
		i += 4
		if v >= 0xdc00 && v <= 0xdfff {
			return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "unpaired Unicode escape")
		}
		if v >= 0xd800 && v <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "unpaired Unicode escape")
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "unpaired Unicode escape")
			}
			i += 6
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if e := strictTranscriptValue(d, 0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "trailing native JSON")
	}
	return nil
}
func strictTranscriptValue(d *json.Decoder, depth int) error {
	if depth > 256 {
		return transcriptFailure("TRANSCRIPT_LIMIT_EXCEEDED", "native JSON nesting exceeds capacity")
	}
	token, e := d.Token()
	if e != nil {
		return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "malformed native JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "duplicate native JSON object key")
			}
			seen[name] = true
			if e := strictTranscriptValue(d, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "malformed native object")
		}
	case '[':
		for d.More() {
			if e := strictTranscriptValue(d, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "malformed native array")
		}
	default:
		return transcriptFailure("TRANSCRIPT_RECORD_INVALID", "invalid native delimiter")
	}
	return nil
}
