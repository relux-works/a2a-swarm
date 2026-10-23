package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

const attachmentEncodingVersion = "a2a.attachment.encoding/1"
const attachmentEncodingReason = "matrix-construct/tuwunel#666"
const attachmentMaxLimit int64 = 1 << 40
const attachmentMaxRatio int64 = 1024

// AttachmentOriginal describes the decoded bytes, never the stored media.
type AttachmentOriginal struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	MimeType string `json:"mimetype"`
}

type attachmentEncoding struct {
	Version  string             `json:"version"`
	Coding   string             `json:"coding"`
	Reason   string             `json:"reason,omitempty"`
	Original AttachmentOriginal `json:"original"`
}

// Raw descriptors preserve presence (including null) and opaque future
// versions. Maps select one raw value per key; the validated fields below are
// also the values returned for consumption. Decode only version until the
// descriptor is known to be v1.
func parseAttachmentEncoding(raw json.RawMessage, limit int64) (*attachmentEncoding, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	malformed := func(field string) error { return fmt.Errorf("fetch: malformed attachment encoding: invalid %s", field) }
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, malformed("encoding object")
	}
	version, ok := encodingString(object["version"])
	if !ok || version == "" {
		return nil, malformed("version")
	}
	result := &attachmentEncoding{Version: version}
	if version != attachmentEncodingVersion {
		return result, nil
	}
	result.Coding, ok = encodingString(object["coding"])
	if !ok || result.Coding == "" {
		return nil, malformed("coding")
	}
	var original map[string]json.RawMessage
	if json.Unmarshal(object["original"], &original) != nil || original == nil {
		return nil, malformed("original object")
	}
	result.Original.Name, ok = encodingString(original["name"])
	if !ok || safeFileName(result.Original.Name) != nil {
		return nil, malformed("original.name")
	}
	var size *int64
	if json.Unmarshal(original["size"], &size) != nil || size == nil {
		return nil, malformed("original.size")
	}
	result.Original.Size = *size
	if *size < 0 || *size > limit {
		return nil, malformed("original.size (outside limit)")
	}
	result.Original.SHA256, ok = encodingString(original["sha256"])
	if !ok || !sha256RE.MatchString(result.Original.SHA256) {
		return nil, malformed("original.sha256")
	}
	result.Original.MimeType, ok = encodingString(original["mimetype"])
	if !ok {
		return nil, malformed("original.mimetype")
	}
	if value, present := object["reason"]; present {
		result.Reason, ok = encodingString(value)
		if !ok {
			return nil, malformed("reason")
		}
	}
	return result, nil
}

// JSON null must not silently become an empty but valid string.
func encodingString(raw json.RawMessage) (string, bool) {
	var value *string
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return "", false
	}
	return *value, true
}

func attachmentLimit(server int64) (int64, error) {
	if server <= 0 {
		return 0, fmt.Errorf("the server states no upload limit; refusing an unbounded media transfer")
	}
	return min(server, attachmentMaxLimit), nil
}

// Both operands are bounded before multiplication or size+1 reads.
func validateGzipSizes(stored, original, limit int64) error {
	if stored < 0 || stored > limit {
		return fmt.Errorf("stored size %d is outside limit %d", stored, limit)
	}
	if original < 0 || original > limit {
		return fmt.Errorf("original size %d is outside limit %d", original, limit)
	}
	if stored == 0 {
		return fmt.Errorf("gzip stored size must be positive")
	}
	if original > attachmentMaxRatio*stored {
		return fmt.Errorf("original size exceeds the 1024:1 expansion ratio")
	}
	return nil
}

// Empty encoding retains legacy output bytes. JSON display keeps the entire
// raw descriptor; text extracts only well-typed version/original metadata.
func attachmentEncodingText(att *Attachment, separator string) string {
	if len(att.Encoding) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(att.Encoding, &object) != nil || object == nil {
		return separator + "encoding=" + sanitizeLine(string(att.Encoding))
	}
	version, _ := encodingString(object["version"])
	coding, _ := encodingString(object["coding"])
	fields := []string{"encoding=" + sanitizeLine(version) + "/" + sanitizeLine(coding)}
	var original map[string]json.RawMessage
	if json.Unmarshal(object["original"], &original) == nil && original != nil {
		if name, ok := encodingString(original["name"]); ok {
			fields = append(fields, "original_name="+sanitizeLine(name))
		}
		var size *int64
		if json.Unmarshal(original["size"], &size) == nil && size != nil {
			fields = append(fields, fmt.Sprintf("original_size=%d", *size))
		}
		if sum, ok := encodingString(original["sha256"]); ok {
			fields = append(fields, "original_sha256="+sanitizeLine(sum))
		}
	}
	return separator + strings.Join(fields, separator)
}
