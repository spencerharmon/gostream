package torr

import (
	"log"
	"os"
	"path/filepath"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/torrent"

	"gostream/internal/gostorm/settings"
)

// The DHT routing table is persisted across restarts so the node rejoins the
// DHT warm (thousands of known-good nodes) instead of re-bootstrapping from a
// handful of public routers, several of which no longer resolve.
const (
	dhtNodesFileName = "dht-nodes.dat"
	dhtNodesMaxSaved = 4096
)

func dhtNodesFile() string {
	return filepath.Join(settings.Path, dhtNodesFileName)
}

func loadPersistedDhtNodes(path string) []dht.Addr {
	nis, err := dht.ReadNodesFromFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("DHT nodes: read %s: %v", path, err)
		}
		return nil
	}
	addrs := make([]dht.Addr, 0, len(nis))
	for _, ni := range nis {
		if ni.Addr.Port == 0 || ni.Addr.IP == nil {
			continue
		}
		addrs = append(addrs, dht.NewAddr(ni.Addr.UDP()))
	}
	log.Printf("DHT nodes: loaded %d persisted nodes from %s", len(addrs), path)
	return addrs
}

func writeDhtNodes(path string, nis []krpc.NodeInfo) error {
	if len(nis) > dhtNodesMaxSaved {
		nis = nis[:dhtNodesMaxSaved]
	}
	tmp := path + ".tmp"
	if err := dht.WriteNodesToFile(nis, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func collectDhtNodes(cl *torrent.Client) []krpc.NodeInfo {
	var nis []krpc.NodeInfo
	for _, s := range cl.DhtServers() {
		if w, ok := s.(torrent.AnacrolixDhtServerWrapper); ok && w.Server != nil {
			nis = append(nis, w.Server.Nodes()...)
		}
	}
	return nis
}

// saveDhtNodes snapshots the routing table to disk. Caller must not hold bt.mu.
func (bt *BTServer) saveDhtNodes() {
	bt.mu.Lock()
	cl := bt.client
	bt.mu.Unlock()
	if cl == nil || settings.BTsets == nil || settings.BTsets.DisableDHT {
		return
	}
	saveDhtNodesFrom(cl)
}

func saveDhtNodesFrom(cl *torrent.Client) {
	nis := collectDhtNodes(cl)
	if len(nis) == 0 {
		return
	}
	if err := writeDhtNodes(dhtNodesFile(), nis); err != nil {
		log.Printf("DHT nodes: save: %v", err)
	}
}
