package telemetry

import "time"

// ObservePoll records one polling round (poller.Options.Observe). Nil-safe.
func (m *Metrics) ObservePoll(_ time.Duration, jobs int, err error) {
	if m == nil {
		return
	}
	now := float64(time.Now().UnixNano()) / 1e9
	m.PollJobs.Add(float64(jobs))
	m.PollLastRound.Set(now)
	if err != nil {
		m.PollTicks.With("error").Inc()
		return
	}
	m.PollTicks.With("ok").Inc()
	m.PollLastSuccess.Set(now)
}

// SetPollInterval exposes the configured gitlab.poll_interval: the polling
// alerts (deploy/prometheus) derive their windows from it. Nil-safe.
func (m *Metrics) SetPollInterval(d time.Duration) {
	if m == nil {
		return
	}
	m.PollInterval.Set(d.Seconds())
}
