package torr

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/anacrolix/dht/v2/krpc"
)

func nodeInfo(ip string, port int, idByte byte) krpc.NodeInfo {
	var id krpc.ID
	id[0] = idByte
	return krpc.NodeInfo{ID: id, Addr: krpc.NodeAddr{IP: net.ParseIP(ip), Port: port}}
}

func TestDhtNodesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), dhtNodesFileName)
	in := []krpc.NodeInfo{
		nodeInfo("192.0.2.1", 6881, 1),
		nodeInfo("192.0.2.2", 51413, 2),
		nodeInfo("2001:db8::1", 6881, 3),
	}
	if err := writeDhtNodes(path, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := loadPersistedDhtNodes(path)
	if len(got) != len(in) {
		t.Fatalf("loaded %d nodes, want %d", len(got), len(in))
	}
	want := map[string]bool{"192.0.2.1:6881": true, "192.0.2.2:51413": true, "[2001:db8::1]:6881": true}
	for _, a := range got {
		if !want[a.String()] {
			t.Errorf("unexpected node %s", a.String())
		}
	}
}

func TestDhtNodesSkipsInvalidAndCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), dhtNodesFileName)
	var in []krpc.NodeInfo
	in = append(in, nodeInfo("192.0.2.9", 0, 9))
	for i := 0; i < dhtNodesMaxSaved+10; i++ {
		in = append(in, nodeInfo("198.51.100.1", 1000+i, byte(i)))
	}
	if err := writeDhtNodes(path, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := loadPersistedDhtNodes(path)
	if len(got) != dhtNodesMaxSaved-1 {
		t.Fatalf("loaded %d nodes, want %d (cap minus the zero-port entry)", len(got), dhtNodesMaxSaved-1)
	}
}

func TestDhtNodesMissingFile(t *testing.T) {
	if got := loadPersistedDhtNodes(filepath.Join(t.TempDir(), "absent.dat")); got != nil {
		t.Fatalf("missing file returned %d nodes, want nil", len(got))
	}
}
