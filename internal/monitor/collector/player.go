package collector

import "time"

// StreamActivity records FUSE demand, not the upstream prefetch pump. Its owner
// must serialize access. Only successful nonempty reads advance LastReadAt.
type StreamActivity struct {
	OpenedAt      time.Time
	LastRequestAt time.Time
	PendingSince  time.Time
	LastReadAt    *time.Time
	ReadOffset    *int64
	sequence      uint64
}

func (s *StreamActivity) BeginRead(off int64, now time.Time) uint64 {
	s.sequence++
	s.LastRequestAt = now
	s.PendingSince = now
	s.ReadOffset = &off
	return s.sequence
}

func (s *StreamActivity) EndRead(sequence uint64, n int, now time.Time) {
	if n > 0 {
		s.LastReadAt = &now
	}
	// Concurrent probes must not clear the most recently requested read's wait
	// or move its cursor to an unrelated offset.
	if sequence == s.sequence {
		s.PendingSince = time.Time{}
		if n > 0 && s.ReadOffset != nil {
			off := *s.ReadOffset + int64(n)
			s.ReadOffset = &off
		}
	}
}

// StreamStats is a sampled active FUSE handle. Buffer fields refer to ReadOffset
// at SampledAt; they are not a total-cache-size approximation.
type StreamStats struct {
	StreamActivity
	Active             bool
	BufferedAheadBytes *int64
	BitrateBytesPerSec *float64
	SampledAt          *time.Time
}

// SetStreamSampler installs the FUSE adapter before Run starts. Sampling shares
// the existing collector loop: requests never enumerate handles/cache chunks.
func (c *Collector) SetStreamSampler(sample func() map[string]StreamStats) {
	c.streamSampler = sample
}

func (c *Collector) collectStreams() {
	if c.streamSampler == nil {
		return
	}
	streams := c.streamSampler()
	c.mu.Lock()
	c.streams = streams
	c.mu.Unlock()
}

func (c *Collector) PlayerStream(hash string) StreamStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.streams[hash]
}
