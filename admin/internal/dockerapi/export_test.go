package dockerapi

import (
	"net/http"
	"time"
)

// NewClientWithHTTP builds a client using h's transport with fast backoffs.
func NewClientWithHTTP(h *http.Client) *Client {
	c := NewClientWithTransport(h.Transport)
	c.retryBackoff = time.Millisecond
	c.eventsBackoff = 20 * time.Millisecond
	return c
}

// SetEventsBackoff overrides the events reconnect delay.
func (c *Client) SetEventsBackoff(d time.Duration) { c.eventsBackoff = d }
