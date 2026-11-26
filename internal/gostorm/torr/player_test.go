package torr

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestPlayerStatsLiveEngine(t *testing.T) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.NoDHT, cfg.DisableTrackers, cfg.NoDefaultPortForwarding = true, true, true
	cfg.DisableTCP, cfg.DisableUTP = true, true
	cfg.DisableWebtorrent = true
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	infoBytes, err := bencode.Marshal(metainfo.Info{Name: "example.mkv", Length: 8, PieceLength: 8, Pieces: make([]byte, 20)})
	if err != nil {
		t.Fatal(err)
	}
	hash := metainfo.HashBytes(infoBytes)
	engine, _ := client.AddTorrentInfoHash(hash)
	tor := &Torrent{Torrent: engine, TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, lastTimeSpeed: time.Now().Add(-time.Second), expiredTime: time.Now().Add(time.Hour)}
	bt := &BTServer{torrents: map[metainfo.Hash]*Torrent{hash: tor}}
	btsMu.Lock()
	old := bts
	bts = bt
	btsMu.Unlock()
	t.Cleanup(func() { btsMu.Lock(); bts = old; btsMu.Unlock() })
	before := tor.expiredTime
	if sample, ok := PlayerStats(hash.HexString()); !ok || sample.Peers != nil || sample.Size != nil {
		t.Fatalf("unsampled must be null: %+v, %v", sample, ok)
	}
	tor.UpdateStats()
	sample, ok := PlayerStats(hash.HexString())
	if !ok || sample.Peers == nil || *sample.Peers != 0 || sample.Seeders == nil || *sample.Seeders != 0 || sample.DownloadRateBytesPerSec == nil || *sample.DownloadRateBytesPerSec != 0 {
		t.Fatalf("actual peerless engine counters: %+v", sample)
	}
	if sample.Size != nil || sample.BytesCompleted != nil || sample.TorrentSampledAt == nil {
		t.Fatalf("no metadata must be null: %+v", sample)
	}
	// Snapshots are immutable across tick replacement, not pointers to mutable fields.
	rate := *sample.DownloadRateBytesPerSec
	tor.UpdateStats()
	if *sample.DownloadRateBytesPerSec != rate {
		t.Fatal("old sample mutated")
	}
	if err := engine.SetInfoBytes(infoBytes); err != nil {
		t.Fatal(err)
	}
	tor.UpdateStats()
	withInfo, _ := PlayerStats(hash.HexString())
	if withInfo.Size == nil || *withInfo.Size != 8 || withInfo.BytesCompleted == nil || *withInfo.BytesCompleted != 0 {
		t.Fatalf("metadata not sampled: %+v", withInfo)
	}
	for i := 0; i < 1000; i++ {
		PlayerStats(hash.HexString())
	}
	if tor.expiredTime != before {
		t.Fatal("polling extended torrent expiry")
	}
	bt.mu.Lock()
	delete(bt.torrents, hash)
	bt.mu.Unlock()
	if _, ok := PlayerStats(hash.HexString()); ok {
		t.Fatal("removed torrent retained")
	}
	for _, invalid := range []string{"", "zzz", "abcdef", "ggcdef0123456789abcdef0123456789abcdef01"} {
		if _, ok := PlayerStats(invalid); ok {
			t.Fatal("invalid hash found")
		}
	}
}
