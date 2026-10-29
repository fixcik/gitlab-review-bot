package gitlab

import "net/http"

// Observer is told about every HTTP attempt the client makes, retries
// included: the request method and the response status code (0 when the
// request failed without a response: connection error, timeout, cancel).
// It must be fast and must not block.
type Observer func(method string, status int)

// SetObserver installs (or, with nil, removes) the observer. Safe to call
// concurrently with requests; meant to be called once after New.
func (c *Client) SetObserver(o Observer) {
	if o == nil {
		c.observer.Store(nil)
		return
	}
	c.observer.Store(&o)
}

func (c *Client) observe(method string, resp *http.Response, err error) {
	o := c.observer.Load()
	if o == nil {
		return
	}
	status := 0
	if err == nil && resp != nil {
		status = resp.StatusCode
	}
	(*o)(method, status)
}
