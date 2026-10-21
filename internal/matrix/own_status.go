package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ForOwnStatus scopes the response evidence contract to the own-status PUT and
// its alias resolution. Other Matrix operations retain their existing behavior.
func (c *Client) ForOwnStatus() *Client {
	copy := c.WithoutRedirects()
	copy.ownStatusEvidence = true
	return copy
}

// decodeOwnStatusResponse is the shared positive evidence predicate for both
// response shapes. send has already required a direct original 2xx and refused
// redirects. No caller may interpret a decoded prefix or absent ID as success.
func decodeOwnStatusResponse(ctx context.Context, resp *http.Response, field string, out any) error {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("own status: read response: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("own status: response interrupted: %w", err)
	}
	if resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {
		return errors.New("own status: incomplete response length")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("own status: response must be exactly one JSON object")
	}
	var id string
	if err := json.Unmarshal(object[field], &id); err != nil || id == "" {
		return fmt.Errorf("own status: response requires nonempty string %s", field)
	}
	return json.Unmarshal(data, out)
}
