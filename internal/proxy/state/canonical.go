package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// canonicalJSON implements the RFC 8785 rules needed by the frozen state
// records: sorted object properties, compact JSON, JSON number validation, and
// ECMAScript-compatible string escaping. Persisted record keys are ASCII, so
// byte ordering and UTF-16 ordering are identical for keys.
func canonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := writeCanonical(&output, decoded); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeCanonical(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		output.WriteString(strconv.FormatBool(typed))
	case string:
		writeJSONString(output, typed)
	case json.Number:
		// State schemas contain integers only. Refuse alternative spellings that
		// could make a semantically identical record hash differently.
		integer, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil || strconv.FormatInt(integer, 10) != typed.String() {
			return fmt.Errorf("canonical JSON only accepts normalized integers: %q", typed)
		}
		output.WriteString(typed.String())
	case []any:
		output.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonical(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				output.WriteByte(',')
			}
			writeJSONString(output, key)
			output.WriteByte(':')
			if err := writeCanonical(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON type %T", value)
	}
	return nil
}

func writeJSONString(output io.ByteWriter, value string) {
	_ = output.WriteByte('"')
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		value = value[size:]
		switch r {
		case '"', '\\':
			_ = output.WriteByte('\\')
			_ = output.WriteByte(byte(r))
		case '\b':
			writeBytes(output, `\b`)
		case '\f':
			writeBytes(output, `\f`)
		case '\n':
			writeBytes(output, `\n`)
		case '\r':
			writeBytes(output, `\r`)
		case '\t':
			writeBytes(output, `\t`)
		default:
			if r < 0x20 {
				writeBytes(output, fmt.Sprintf(`\u%04x`, r))
			} else {
				writeBytes(output, string(r))
			}
		}
	}
	_ = output.WriteByte('"')
}

func writeBytes(output io.ByteWriter, value string) {
	for i := range len(value) {
		_ = output.WriteByte(value[i])
	}
}

func checksumRecord(value any) (string, []byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return "", nil, err
	}
	delete(object, "checksum")
	canonical, err := canonicalJSON(object)
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), canonical, nil
}

func marshalChecksummed(value any, setChecksum func(string)) ([]byte, error) {
	checksum, _, err := checksumRecord(value)
	if err != nil {
		return nil, err
	}
	setChecksum(checksum)
	return canonicalJSON(value)
}

func verifyChecksum(value any, expected string) error {
	actual, _, err := checksumRecord(value)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected) || actual != expected {
		return fmt.Errorf("%w: checksum mismatch", ErrInvalidState)
	}
	return nil
}
