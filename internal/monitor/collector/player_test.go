package collector

import (
	"testing"
	"time"
)

func TestStreamActivity(t *testing.T) {
	now := time.Now()
	var s StreamActivity
	first := s.BeginRead(100, now)
	if s.LastReadAt != nil || *s.ReadOffset != 100 || s.PendingSince != now {
		t.Fatalf("begin: %+v", s)
	}
	second := s.BeginRead(200, now.Add(time.Second))
	s.EndRead(first, 20, now.Add(2*time.Second))
	if *s.ReadOffset != 200 || s.PendingSince.IsZero() {
		t.Fatal("old concurrent read changed latest cursor/wait")
	}
	s.EndRead(second, 0, now.Add(3*time.Second))
	if *s.ReadOffset != 200 || !s.PendingSince.IsZero() || !s.LastReadAt.Equal(now.Add(2*time.Second)) {
		t.Fatal("empty/failed read was counted as success")
	}
	third := s.BeginRead(0, now.Add(4*time.Second)) // backward seek
	s.EndRead(third, 30, now.Add(5*time.Second))
	if *s.ReadOffset != 30 {
		t.Fatalf("seek cursor=%d", *s.ReadOffset)
	}
}

func TestPlayerStreamIndexedSamplesReplace(t *testing.T) {
	c := &Collector{}
	calls := 0
	c.SetStreamSampler(func() map[string]StreamStats {
		calls++
		if calls == 1 {
			return map[string]StreamStats{"hash": {Active: true}}
		}
		return nil // handle released
	})
	c.collectStreams()
	for i := 0; i < 1000; i++ {
		if !c.PlayerStream("hash").Active || c.PlayerStream("unknown").Active {
			t.Fatal("incorrect lookup")
		}
	}
	if calls != 1 {
		t.Fatal("request triggered sampling")
	}
	c.collectStreams()
	if c.PlayerStream("hash").Active {
		t.Fatal("released stream retained")
	}
}
