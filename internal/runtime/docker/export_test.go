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

// RestoreFor sets the budget one undo of an explicit frame runs under.
func (c *Client) RestoreFor(d time.Duration) *Client {
	c.restoreBudget = d
	return c
}

// ReserveRestartFor sets how much of a restore's budget the wait after an unanswered stop leaves
// the restart.
func (c *Client) ReserveRestartFor(d time.Duration) *Client {
	c.restartReserve = d
	return c
}

// WaitBound is waitBound.
var WaitBound = waitBound
