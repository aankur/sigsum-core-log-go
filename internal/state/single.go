package state

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"

	"git.sigsum.org/log-go/internal/db"
	"git.sigsum.org/sigsum-go/pkg/client"
	"git.sigsum.org/sigsum-go/pkg/log"
	"git.sigsum.org/sigsum-go/pkg/merkle"
	"git.sigsum.org/sigsum-go/pkg/requests"
	"git.sigsum.org/sigsum-go/pkg/types"
)

// StateManagerSingle implements a single-instance StateManagerPrimary for primary nodes
type StateManagerSingle struct {
	client    db.Client
	signer    crypto.Signer
	namespace merkle.Hash
	interval  time.Duration
	deadline  time.Duration
	secondary client.Client

	// Lock-protected access to pointers.  A write lock is only obtained once
	// per interval when doing pointer rotation.  All endpoints are readers.
	sync.RWMutex
	signedTreeHead   *types.SignedTreeHead
	cosignedTreeHead *types.CosignedTreeHead

	// Syncronized and deduplicated witness cosignatures for signedTreeHead
	events       chan *event
	cosignatures map[merkle.Hash]*types.Signature
}

// NewStateManagerSingle() sets up a new state manager, in particular its
// signedTreeHead.  An optional secondary node can be used to ensure that
// a newer primary tree is not signed unless it has been replicated.
func NewStateManagerSingle(dbcli db.Client, signer crypto.Signer, interval, deadline time.Duration, secondary client.Client) (*StateManagerSingle, error) {
	sm := &StateManagerSingle{
		client:    dbcli,
		signer:    signer,
		namespace: *merkle.HashFn(signer.Public().(ed25519.PublicKey)),
		interval:  interval,
		deadline:  deadline,
		secondary: secondary,
	}
	sth, err := sm.restoreTreeHead()
	if err != nil {
		return nil, fmt.Errorf("restore signed tree head: %v", err)
	}
	sm.signedTreeHead = sth

	ictx, cancel := context.WithTimeout(context.Background(), sm.deadline)
	defer cancel()
	return sm, sm.tryRotate(ictx)
}

func (sm *StateManagerSingle) ToCosignTreeHead() *types.SignedTreeHead {
	sm.RLock()
	defer sm.RUnlock()
	return sm.signedTreeHead
}

func (sm *StateManagerSingle) CosignedTreeHead(_ context.Context) (*types.CosignedTreeHead, error) {
	sm.RLock()
	defer sm.RUnlock()
	if sm.cosignedTreeHead == nil {
		return nil, fmt.Errorf("no cosignatures available")
	}
	return sm.cosignedTreeHead, nil
}

func (sm *StateManagerSingle) AddCosignature(ctx context.Context, pub *types.PublicKey, sig *types.Signature) error {
	sm.RLock()
	defer sm.RUnlock()

	msg := sm.signedTreeHead.TreeHead.ToBinary(&sm.namespace)
	if !ed25519.Verify(ed25519.PublicKey(pub[:]), msg, sig[:]) {
		return fmt.Errorf("invalid cosignature")
	}
	select {
	case sm.events <- &event{merkle.HashFn(pub[:]), sig}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("request timeout")
	}
}

func (sm *StateManagerSingle) Run(ctx context.Context) {
	sm.events = make(chan *event, 4096)
	defer close(sm.events)
	ticker := time.NewTicker(sm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ictx, cancel := context.WithTimeout(ctx, sm.deadline)
			defer cancel()
			if err := sm.tryRotate(ictx); err != nil {
				log.Warning("failed rotating tree heads: %v", err)
			}
		case ev := <-sm.events:
			sm.handleEvent(ev)
		case <-ctx.Done():
			return
		}
	}
}

func (sm *StateManagerSingle) tryRotate(ctx context.Context) error {
	th, err := sm.client.GetTreeHead(ctx)
	if err != nil {
		return fmt.Errorf("get tree head: %v", err)
	}
	nextSTH, err := sm.chooseTree(ctx, th).Sign(sm.signer, &sm.namespace)
	if err != nil {
		return fmt.Errorf("sign tree head: %v", err)
	}
	log.Debug("wanted to advance to size %d, chose size %d", th.TreeSize, nextSTH.TreeSize)

	sm.rotate(nextSTH)
	return nil
}

// chooseTree picks a tree to publish, taking the state of a possible secondary node into account.
func (sm *StateManagerSingle) chooseTree(ctx context.Context, proposedTreeHead *types.TreeHead) *types.TreeHead {
	if !sm.secondary.Initiated() {
		return proposedTreeHead
	}

	secSTH, err := sm.secondary.GetToCosignTreeHead(ctx)
	if err != nil {
		log.Warning("failed fetching tree head from secondary: %v", err)
		return refreshTreeHead(sm.signedTreeHead.TreeHead)
	}
	if secSTH.TreeSize > proposedTreeHead.TreeSize {
		log.Error("secondary is ahead of us: %d > %d", secSTH.TreeSize, proposedTreeHead.TreeSize)
		return refreshTreeHead(sm.signedTreeHead.TreeHead)
	}

	if secSTH.TreeSize == proposedTreeHead.TreeSize {
		if secSTH.RootHash != proposedTreeHead.RootHash {
			log.Error("secondary root hash doesn't match our root hash at tree size %d", secSTH.TreeSize)
			return refreshTreeHead(sm.signedTreeHead.TreeHead)
		}
		log.Debug("secondary is up-to-date with matching tree head, using proposed tree, size %d", proposedTreeHead.TreeSize)
		return proposedTreeHead
	}
	//
	// Now we know that the proposed tree size is larger than the secondary's tree size.
	// We also now that the secondary's minimum tree size is 0.
	// This means that the proposed tree size is at least 1.
	//
	// Case 1: secondary tree size is 0, primary tree size is >0 --> return based on what we signed before
	// Case 2: secondary tree size is 1, primary tree size is >1 --> fetch consistency proof, if ok ->
	//   2a) secondary tree size is smaller than or equal to what we than signed before -> return whatever we signed before
	//   2b) secondary tree size is larger than what we signed before -> return secondary tree head
	//
	// (If not ok in case 2, return based on what we signed before)
	//
	if secSTH.TreeSize == 0 {
		return refreshTreeHead(sm.signedTreeHead.TreeHead)
	}
	if err := sm.verifyConsistencyWithLatest(ctx, secSTH.TreeHead); err != nil {
		log.Error("secondaries tree not consistent with ours: %v", err)
		return refreshTreeHead(sm.signedTreeHead.TreeHead)
	}
	if secSTH.TreeSize <= sm.signedTreeHead.TreeSize {
		log.Warning("secondary is behind what primary already signed: %d <= %d", secSTH.TreeSize, sm.signedTreeHead.TreeSize)
		return refreshTreeHead(sm.signedTreeHead.TreeHead)
	}

	log.Debug("using latest tree head from secondary: size %d", secSTH.TreeSize)
	return refreshTreeHead(secSTH.TreeHead)
}

func (sm *StateManagerSingle) verifyConsistencyWithLatest(ctx context.Context, to types.TreeHead) error {
	from := sm.signedTreeHead.TreeHead
	req := &requests.ConsistencyProof{
		OldSize: from.TreeSize,
		NewSize: to.TreeSize,
	}
	proof, err := sm.client.GetConsistencyProof(ctx, req)
	if err != nil {
		return fmt.Errorf("unable to get consistency proof from %d to %d: %w", req.OldSize, req.NewSize, err)
	}
	if err := proof.Verify(&from.RootHash, &to.RootHash); err != nil {
		return fmt.Errorf("invalid consistency proof from %d to %d: %v", req.OldSize, req.NewSize, err)
	}
	log.Debug("consistency proof from %d to %d verified", req.OldSize, req.NewSize)
	return nil
}

func (sm *StateManagerSingle) rotate(nextSTH *types.SignedTreeHead) {
	sm.Lock()
	defer sm.Unlock()

	log.Debug("about to rotate tree heads, next at %d: %s", nextSTH.TreeSize, sm.treeStatusString())
	sm.handleEvents()
	sm.setCosignedTreeHead()
	sm.setToCosignTreeHead(nextSTH)
	log.Debug("tree heads rotated: %s", sm.treeStatusString())
}

func (sm *StateManagerSingle) handleEvents() {
	log.Debug("handling any outstanding events")
	for i, n := 0, len(sm.events); i < n; i++ {
		sm.handleEvent(<-sm.events)
	}
}

func (sm *StateManagerSingle) handleEvent(ev *event) {
	log.Debug("handling event from witness %x", ev.keyHash[:])
	sm.cosignatures[*ev.keyHash] = ev.cosignature
}

func (sm *StateManagerSingle) setCosignedTreeHead() {
	n := len(sm.cosignatures)
	if n == 0 {
		sm.cosignedTreeHead = nil
		return
	}

	var cth types.CosignedTreeHead
	cth.SignedTreeHead = *sm.signedTreeHead
	cth.Cosignature = make([]types.Signature, 0, n)
	cth.KeyHash = make([]merkle.Hash, 0, n)
	for keyHash, cosignature := range sm.cosignatures {
		cth.KeyHash = append(cth.KeyHash, keyHash)
		cth.Cosignature = append(cth.Cosignature, *cosignature)
	}
	sm.cosignedTreeHead = &cth
}

func (sm *StateManagerSingle) setToCosignTreeHead(nextSTH *types.SignedTreeHead) {
	sm.cosignatures = make(map[merkle.Hash]*types.Signature)
	sm.signedTreeHead = nextSTH
}

func (sm *StateManagerSingle) treeStatusString() string {
	var cosigned uint64
	if sm.cosignedTreeHead != nil {
		cosigned = sm.cosignedTreeHead.TreeSize
	}
	return fmt.Sprintf("signed at %d, cosigned at %d", sm.signedTreeHead.TreeSize, cosigned)
}

func (sm *StateManagerSingle) restoreTreeHead() (*types.SignedTreeHead, error) {
	th := zeroTreeHead() // TODO: restore from disk, stored when advanced the tree; zero tree head if "bootstrap"
	return refreshTreeHead(*th).Sign(sm.signer, &sm.namespace)
}

func zeroTreeHead() *types.TreeHead {
	return refreshTreeHead(types.TreeHead{RootHash: *merkle.HashFn([]byte(""))})
}

func refreshTreeHead(th types.TreeHead) *types.TreeHead {
	th.Timestamp = uint64(time.Now().Unix())
	return &th
}