package raft

import (
	"fmt"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/snap"
	"github.com/coreos/etcd/wal/walpb"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/rlp"
	"gopkg.in/fatih/set.v0"
	"io"
	"log"
	"math/big"
	"sort"
	"time"
)

// Snapshot

type Snapshot struct {
	addresses      []Address
	removedRaftIds []uint16 // Raft IDs for permanently removed peers
	headBlockHash  common.Hash
}

type ByRaftId []Address

func (a ByRaftId) Len() int           { return len(a) }
func (a ByRaftId) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByRaftId) Less(i, j int) bool { return a[i].raftId < a[j].raftId }

func (pm *ProtocolManager) buildSnapshot() *Snapshot {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	numNodes := len(pm.confState.Nodes)

	snapshot := &Snapshot{
		addresses:     make([]Address, numNodes),
		headBlockHash: pm.blockchain.CurrentBlock().Hash(),
	}

	for i, rawRaftId := range pm.confState.Nodes {
		raftId := uint16(rawRaftId)

		if raftId == pm.raftId {
			snapshot.addresses[i] = *pm.address
		} else {
			snapshot.addresses[i] = *pm.peers[raftId].address
		}
	}

	sort.Sort(ByRaftId(snapshot.addresses))

	return snapshot
}

func (pm *ProtocolManager) triggerSnapshot() {
	glog.V(logger.Info).Infof("start snapshot [applied index: %d | last snapshot index: %d]", pm.appliedIndex, pm.snapshotIndex)

	snapData := pm.buildSnapshot().toBytes()
	snap, err := pm.raftStorage.CreateSnapshot(pm.appliedIndex, &pm.confState, snapData)
	if err != nil {
		panic(err)
	}
	if err := pm.saveRaftSnapshot(snap); err != nil {
		panic(err)
	}
	// Discard all log entries prior to appliedIndex.
	if err := pm.raftStorage.Compact(pm.appliedIndex); err != nil {
		panic(err)
	}
	glog.V(logger.Info).Infof("compacted log at index %d", pm.appliedIndex)
	pm.snapshotIndex = pm.appliedIndex
}

func confStateIdSet(confState raftpb.ConfState) *set.Set {
	set := set.New()
	for _, rawRaftId := range confState.Nodes {
		set.Add(uint16(rawRaftId))
	}
	return set
}

func (pm *ProtocolManager) updateClusterMembership(newConfState raftpb.ConfState, addresses []Address, removedRaftIds []uint16) {
	glog.V(logger.Info).Infof("updating cluster membership per raft snapshot")

	prevConfState := pm.confState

	pm.mu.Lock()
	pm.removedPeers = set.New()
	for _, removedRaftId := range removedRaftIds {
		pm.removedPeers.Add(removedRaftId)
	}
	pm.mu.Unlock()

	prevIds := confStateIdSet(prevConfState)
	newIds := confStateIdSet(newConfState)
	idsToRemove := set.Difference(prevIds, newIds)
	for _, idIfaceToRemove := range idsToRemove.List() {
		raftId := idIfaceToRemove.(uint16)
		glog.V(logger.Info).Infof("removing old raft peer %v", raftId)

		pm.removePeer(raftId)
	}

	for _, address := range addresses {
		if address.raftId == pm.raftId {
			pm.mu.Lock()
			// If we're a newcomer to an existing cluster, this is where we learn
			// our own Address.
			pm.address = &address
			pm.mu.Unlock()
		} else {
			pm.mu.RLock()
			existingPeer := pm.peers[address.raftId]
			pm.mu.RUnlock()

			if existingPeer == nil {
				glog.V(logger.Info).Infof("adding new raft peer %v", address.raftId)
				pm.addPeer(&address)
			}
		}
	}

	pm.mu.Lock()
	pm.confState = newConfState
	pm.mu.Unlock()

	glog.V(logger.Info).Infof("updated cluster membership")
}

// For persisting cluster membership changes correctly, we need to trigger a
// snapshot before advancing our persisted appliedIndex in LevelDB.
//
// See handling of EntryConfChange entries in raft/handler.go for details.
func (pm *ProtocolManager) triggerSnapshotWithNextIndex(index uint64) {
	pm.appliedIndex = index
	pm.triggerSnapshot()
}

func (pm *ProtocolManager) maybeTriggerSnapshot() {
	if pm.appliedIndex-pm.snapshotIndex < snapshotPeriod {
		return
	}

	pm.triggerSnapshot()
}

func (pm *ProtocolManager) loadSnapshot() {
	if raftSnapshot := pm.readRaftSnapshot(); raftSnapshot != nil {
		glog.V(logger.Info).Infof("loading snapshot")

		pm.applyRaftSnapshot(*raftSnapshot)
	} else {
		glog.V(logger.Info).Infof("no snapshot to load")
	}
}

func (snapshot *Snapshot) toBytes() []byte {
	size, r, err := rlp.EncodeToReader(snapshot)
	if err != nil {
		panic(fmt.Sprintf("error: failed to RLP-encode Snapshot: %s", err.Error()))
	}
	var buffer = make([]byte, uint32(size))
	r.Read(buffer)

	return buffer
}

func bytesToSnapshot(bytes []byte) *Snapshot {
	var snapshot Snapshot
	if err := rlp.DecodeBytes(bytes, &snapshot); err != nil {
		log.Fatalf("failed to RLP-decode Snapshot: %v", err)
	}
	return &snapshot
}

func (snapshot *Snapshot) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{snapshot.addresses, snapshot.removedRaftIds, snapshot.headBlockHash})
}

func (snapshot *Snapshot) DecodeRLP(s *rlp.Stream) error {
	// These fields need to be public:
	var temp struct {
		Addresses      []Address
		RemovedRaftIds []uint16
		HeadBlockHash  common.Hash
	}

	if err := s.Decode(&temp); err != nil {
		return err
	} else {
		snapshot.addresses, snapshot.removedRaftIds, snapshot.headBlockHash = temp.Addresses, temp.RemovedRaftIds, temp.HeadBlockHash
		return nil
	}
}

// Raft snapshot

func (pm *ProtocolManager) saveRaftSnapshot(snap raftpb.Snapshot) error {
	if err := pm.snapshotter.SaveSnap(snap); err != nil {
		return err
	}

	walSnap := walpb.Snapshot{
		Index: snap.Metadata.Index,
		Term:  snap.Metadata.Term,
	}

	if err := pm.wal.SaveSnapshot(walSnap); err != nil {
		return err
	}

	return pm.wal.ReleaseLockTo(snap.Metadata.Index)
}

func (pm *ProtocolManager) readRaftSnapshot() *raftpb.Snapshot {
	snapshot, err := pm.snapshotter.Load()
	if err != nil && err != snap.ErrNoSnapshot {
		glog.Fatalf("error loading snapshot: %v", err)
	}

	return snapshot
}

func (pm *ProtocolManager) applyRaftSnapshot(raftSnapshot raftpb.Snapshot) {
	glog.V(logger.Info).Infof("applying snapshot to raft storage")
	if err := pm.raftStorage.ApplySnapshot(raftSnapshot); err != nil {
		glog.Fatalln("failed to apply snapshot: ", err)
	}
	snapshot := bytesToSnapshot(raftSnapshot.Data)

	latestBlockHash := snapshot.headBlockHash

	pm.updateClusterMembership(raftSnapshot.Metadata.ConfState, snapshot.addresses, snapshot.removedRaftIds)

	preSyncHead := pm.blockchain.CurrentBlock()

	glog.V(logger.Info).Infof("before sync, chain head is at block %x", preSyncHead.Hash())

	if latestBlock := pm.blockchain.GetBlockByHash(latestBlockHash); latestBlock == nil {
		pm.syncBlockchainUntil(latestBlockHash)
		pm.logNewlyAcceptedTransactions(preSyncHead)

		glog.V(logger.Info).Infof("%s: %x\n", chainExtensionMessage, pm.blockchain.CurrentBlock().Hash())
	} else {
		glog.V(logger.Info).Infof("blockchain is caught up; no need to synchronize")
	}

	snapMeta := raftSnapshot.Metadata
	pm.confState = snapMeta.ConfState
	pm.snapshotIndex = snapMeta.Index
	pm.advanceAppliedIndex(snapMeta.Index)
}

func (pm *ProtocolManager) syncBlockchainUntil(hash common.Hash) {
	// We don't need to lock access to pm.peers here; only reads can happen
	// concurrently with this.

	for {
		for peerId, peer := range pm.peers {
			glog.V(logger.Info).Infof("synchronizing with peer %v up to block %x", peerId, hash)

			peerId := peer.p2pNode.ID.String()
			peerIdPrefix := fmt.Sprintf("%x", peer.p2pNode.ID[:8])

			if err := pm.downloader.Synchronise(peerIdPrefix, hash, big.NewInt(0), downloader.BoundedFullSync); err != nil {
				glog.V(logger.Warn).Infof("failed to synchronize with peer %v", peerId)

				time.Sleep(500 * time.Millisecond)
			} else {
				return
			}
		}
	}
}

func (pm *ProtocolManager) logNewlyAcceptedTransactions(preSyncHead *types.Block) {
	newHead := pm.blockchain.CurrentBlock()
	numBlocks := newHead.NumberU64() - preSyncHead.NumberU64()
	blocks := make([]*types.Block, numBlocks)
	currBlock := newHead
	blocksSeen := 0
	for currBlock.Hash() != preSyncHead.Hash() {
		blocks[int(numBlocks)-(1+blocksSeen)] = currBlock

		blocksSeen += 1
		currBlock = pm.blockchain.GetBlockByHash(currBlock.ParentHash())
	}
	for _, block := range blocks {
		for _, tx := range block.Transactions() {
			logger.LogRaftCheckpoint(logger.TxAccepted, tx.Hash().Hex())
		}
	}
}
