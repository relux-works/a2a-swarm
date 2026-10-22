package matrix

// ForOwnStatus keeps the own-status no-redirect provenance policy. All
// server-generated ACK/control responses now share the same strict interpreter.
func (c *Client) ForOwnStatus() *Client { return c.WithoutRedirects() }
