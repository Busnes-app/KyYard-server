package docker

import "time"

// WatchStartFor sets how long an explicit recreate watches its started container, and how often.
func (c *Client) WatchStartFor(window, poll time.Duration) *Client {
	c.startWatch, c.startPoll = window, poll
	return c
}

// WaitStopFor sets how long a restore waits for an old container whose stop went unanswered to
// stop.
func (c *Client) WaitStopFor(d time.Duration) *Client {
	c.stopWait = d
	return c
}
