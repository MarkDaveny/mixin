package kernel

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/config"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/kernel/internal/clock"
	"github.com/MixinNetwork/mixin/logger"
)

const MaxKernelNodesCount = 50

func (node *Node) ElectionLoop() {
	defer close(node.elc)

	ticker := time.NewTicker(time.Duration(node.custom.Node.KernelOprationPeriod) * time.Second)
	defer ticker.Stop()

	chain := node.BootChain(node.IdForNetwork)
	for chain.State == nil {
		select {
		case <-node.done:
			return
		case <-ticker.C:
			err := chain.tryToSendAcceptTransaction()
			if err != nil {
				logger.Println("tryToSendAcceptTransaction", err)
			}
		}
	}
	logger.Println("ElectionLoop ACCEPTED!")

	for {
		select {
		case <-node.done:
			return
		case <-ticker.C:
			err := node.tryToSendRemoveTransaction()
			if err != nil {
				logger.Println("tryToSendRemoveTransaction", err)
			}
		}
	}
}

// TODO slashing rule for failure of this elected node
func (node *Node) electSnapshotNode(operation byte, now uint64) crypto.Hash {
	switch operation {
	case common.TransactionTypeMint:
	case common.TransactionTypeNodeRemove:
	case common.TransactionTypeNodePledge:
	case common.TransactionTypeCustodianUpdateNodes:
	case common.TransactionTypeCustodianSlashNodes:
	default:
		return crypto.Hash{}
	}
	accepted := node.NodesListWithoutState(now, true)
	if len(accepted) < config.KernelMinimumNodesCount {
		panic(len(accepted))
	}
	accepted = accepted[1 : len(accepted)-1]
	day := int((now - node.Epoch) / (uint64(time.Hour) * 24))
	idx := (day + int(operation)) % len(accepted)
	return accepted[idx].IdForNetwork
}

func (node *Node) checkRemovePossibility(nodeId crypto.Hash, now uint64, old *common.VersionedTransaction) (*CNode, error) {
	if p := node.PledgingNode(now); p != nil {
		return nil, fmt.Errorf("still pledging now %s", p.Signer.String())
	}

	if now < node.Epoch {
		return nil, fmt.Errorf("local time invalid %d %d", now, node.Epoch)
	}
	if !node.checkConsensusAcceptHour(now) {
		hour := (now - node.Epoch) / uint64(time.Hour) % 24
		return nil, fmt.Errorf("invalid node remove hour %d", hour)
	}

	var candi *CNode
	var accepted []*CNode
	for _, cn := range node.NodesListWithoutState(now, false) {
		if old != nil && cn.Transaction == old.PayloadHash() {
			candi = cn
			continue
		}
		if now < cn.Timestamp {
			return nil, fmt.Errorf("invalid timestamp %d %d", cn.Timestamp, now)
		}
		elapse := time.Duration(now - cn.Timestamp)
		if elapse < config.KernelNodePledgePeriodMinimum {
			return nil, fmt.Errorf("invalid period %d %d %d %d",
				config.KernelNodePledgePeriodMinimum, elapse, now, cn.Timestamp)
		}
		switch cn.State {
		case common.NodeStateAccepted:
			accepted = append(accepted, cn)
		case common.NodeStateCancelled:
		case common.NodeStateRemoved:
		default:
			return nil, fmt.Errorf("invalid node pending state %s %s", cn.Signer, cn.State)
		}
	}
	if len(accepted) <= config.KernelMinimumNodesCount {
		return nil, fmt.Errorf("all old nodes removed %d", len(accepted))
	}
	if candi == nil {
		candi = accepted[0]
	}
	if candi.IdForNetwork == nodeId {
		return nil, fmt.Errorf("never handle the node remove transaction by the node self")
	}
	return candi, nil
}

func (node *Node) buildNodeRemoveTransaction(nodeId crypto.Hash, timestamp uint64, old *common.VersionedTransaction) (*common.VersionedTransaction, error) {
	candi, err := node.checkRemovePossibility(nodeId, timestamp, old)
	if err != nil {
		return nil, err
	}
	if old != nil && candi.Transaction == old.PayloadHash() {
		return old, nil
	}

	accept, _, err := node.persistStore.ReadTransaction(candi.Transaction)
	if err != nil {
		return nil, err
	}
	if accept == nil {
		return nil, fmt.Errorf("accept transaction not available yet %s", candi.Transaction)
	}
	if accept.PayloadHash() != candi.Transaction {
		return nil, fmt.Errorf("accept transaction malformed %s %s", candi.Transaction, accept.PayloadHash())
	}
	signer := candi.Signer.PublicSpendKey
	payee := candi.Payee.PublicSpendKey[:]
	if len(accept.Extra) != len(signer)*2 {
		return nil, fmt.Errorf("invalid accept transaction extra %s", hex.EncodeToString(accept.Extra))
	}
	if !bytes.Equal(append(signer[:], payee...), accept.Extra) {
		return nil, fmt.Errorf("invalid accept transaction extra %s %s %s",
			hex.EncodeToString(accept.Extra), signer, hex.EncodeToString(payee))
	}

	consensusSnap, _ := node.ReadLastConsensusSnapshotWithHack()
	tx := node.NewTransaction(common.XINAssetId)
	tx.AddInput(candi.Transaction, 0)
	tx.Extra = accept.Extra
	script := common.NewThresholdScript(1)
	in := fmt.Sprintf("NODEREMOVE%s", candi.Signer.String())
	si := crypto.Blake3Hash([]byte(candi.Payee.String() + in))
	seed := append(si[:], si[:]...)
	tx.AddOutputWithType(common.OutputTypeNodeRemove, []*common.Address{&candi.Payee}, script, accept.Outputs[0].Amount, seed)
	tx.References = []crypto.Hash{consensusSnap.SoleTransaction()}

	return tx.AsVersioned(), nil
}

func (node *Node) tryToSendRemoveTransaction() error {
	eid := node.electSnapshotNode(common.TransactionTypeNodeRemove, node.GraphTimestamp)
	if eid != node.IdForNetwork {
		return fmt.Errorf("node remove operation at %d only by %s not me", node.GraphTimestamp, eid)
	}
	tx, err := node.buildNodeRemoveTransaction(node.IdForNetwork, node.GraphTimestamp, nil)
	if err != nil {
		return err
	}
	logger.Verbosef("tryToSendRemoveTransaction %s\n", tx.PayloadHash())

	err = tx.Validate(node.persistStore, node.GraphTimestamp, false)
	if err != nil {
		return err
	}
	err = node.persistStore.CachePutTransaction(tx)
	if err != nil {
		return err
	}
	chain := node.getOrCreateChain(node.IdForNetwork)
	s := &common.Snapshot{
		Version: common.SnapshotVersionCommonEncoding,
		NodeId:  node.IdForNetwork,
	}
	s.AddSoleTransaction(tx.PayloadHash())
	return chain.AppendSelfEmpty(s)
}

func (node *Node) validateNodeRemoveSnapshot(s *common.Snapshot, tx *common.VersionedTransaction, finalized bool) error {
	timestamp := s.Timestamp
	if s.Timestamp == 0 && s.NodeId == node.IdForNetwork {
		timestamp = clock.NowUnixNano()
	}
	eid := node.electSnapshotNode(common.TransactionTypeNodeRemove, timestamp)
	if eid != s.NodeId {
		return fmt.Errorf("node remove operation at %d only by %s not %s", timestamp, eid, s.NodeId)
	}

	signer := tx.NodeTransactionExtraAsSigner()
	id := signer.Hash().ForNetwork(node.networkId)
	for _, cn := range node.allNodesSortedWithState {
		if cn.State != common.NodeStateRemoved {
			continue
		}
		if cn.IdForNetwork != id {
			continue
		}
		if finalized && cn.Transaction == tx.PayloadHash() {
			return nil
		}
	}
	cantx, err := node.buildNodeRemoveTransaction(s.NodeId, timestamp, tx)
	if err != nil {
		return err
	}
	if cantx.PayloadHash() != tx.PayloadHash() {
		return fmt.Errorf("invalid node remove transaction %s %s", cantx.PayloadHash(), tx.PayloadHash())
	}
	return nil
}

func (chain *Chain) checkNodeAcceptPossibility(timestamp uint64, finalized bool) error {
	ci, epoch := chain.ConsensusInfo, chain.node.Epoch
	if chain.State != nil {
		return fmt.Errorf("invalid graph round %s %d", chain.ChainId, chain.State.CacheRound.Number)
	}

	pledging := chain.node.PledgingNode(timestamp)
	if pledging == nil {
		return fmt.Errorf("no consensus pledging node %t", pledging == nil)
	}
	if pledging.Signer.String() != ci.Signer.String() {
		return fmt.Errorf("invalid consensus pledging node %s %s", pledging.Signer, ci.Signer)
	}

	if timestamp < epoch {
		return fmt.Errorf("invalid snapshot timestamp %d %d", epoch, timestamp)
	}

	if !chain.node.checkConsensusAcceptHour(timestamp) {
		hour := (timestamp - epoch) / uint64(time.Hour) % 24
		return fmt.Errorf("invalid node accept hour %d", hour)
	}

	threshold := config.SnapshotRoundGap * config.SnapshotReferenceThreshold
	if !finalized && timestamp+threshold*2 < chain.node.GraphTimestamp {
		return fmt.Errorf("invalid snapshot timestamp %d %d", chain.node.GraphTimestamp, timestamp)
	}

	if timestamp < pledging.Timestamp {
		return fmt.Errorf("invalid snapshot timestamp %d %d", pledging.Timestamp, timestamp)
	}
	elapse := time.Duration(timestamp - pledging.Timestamp)
	if elapse < config.KernelNodeAcceptPeriodMinimum {
		return fmt.Errorf("invalid accept period %d %d", config.KernelNodeAcceptPeriodMinimum, elapse)
	}
	if elapse > config.KernelNodeAcceptPeriodMaximum {
		return fmt.Errorf("invalid accept period %d %d", config.KernelNodeAcceptPeriodMaximum, elapse)
	}

	return nil
}

func (chain *Chain) buildNodeAcceptTransaction(timestamp uint64, finalized bool) (*common.VersionedTransaction, error) {
	err := chain.checkNodeAcceptPossibility(timestamp, finalized)
	if err != nil {
		return nil, err
	}

	ci := chain.ConsensusInfo
	pledge, _, err := chain.node.persistStore.ReadTransaction(ci.Transaction)
	if err != nil {
		return nil, err
	}
	if pledge == nil {
		return nil, fmt.Errorf("pledge transaction not available yet %s", ci.Transaction)
	}
	if pledge.PayloadHash() != ci.Transaction {
		return nil, fmt.Errorf("pledge transaction malformed %s %s", ci.Transaction, pledge.PayloadHash())
	}
	signer := ci.Signer.PublicSpendKey
	if len(pledge.Extra) != len(signer)*2 {
		return nil, fmt.Errorf("invalid pledge transaction extra %s",
			hex.EncodeToString(pledge.Extra))
	}
	if !bytes.Equal(signer[:], pledge.Extra[:len(signer)]) {
		return nil, fmt.Errorf("invalid pledge transaction extra %s %s",
			hex.EncodeToString(pledge.Extra[:len(signer)]), signer)
	}

	consensusSnap, _ := chain.node.ReadLastConsensusSnapshotWithHack()
	tx := chain.node.NewTransaction(common.XINAssetId)
	tx.AddInput(ci.Transaction, 0)
	tx.AddOutputWithType(common.OutputTypeNodeAccept, nil, common.Script{}, pledge.Outputs[0].Amount, []byte{})
	tx.Extra = pledge.Extra
	tx.References = []crypto.Hash{consensusSnap.SoleTransaction()}

	return tx.AsVersioned(), nil
}

func (chain *Chain) tryToSendAcceptTransaction() error {
	now := clock.NowUnixNano()
	ver, err := chain.buildNodeAcceptTransaction(now, false)
	if err != nil {
		return err
	}
	logger.Verbosef("tryToSendAcceptTransaction %s\n", ver.PayloadHash())

	err = ver.Validate(chain.node.persistStore, now, false)
	if err != nil {
		return err
	}
	err = chain.node.persistStore.CachePutTransaction(ver)
	if err != nil {
		return err
	}
	s := &common.Snapshot{
		Version: common.SnapshotVersionCommonEncoding,
		NodeId:  chain.ChainId,
	}
	s.AddSoleTransaction(ver.PayloadHash())
	err = chain.AppendSelfEmpty(s)
	logger.Println("tryToSendAcceptTransaction", ver.PayloadHash(), hex.EncodeToString(ver.Marshal()), err)
	return nil
}

func (node *Node) validateNodeAcceptSnapshot(s *common.Snapshot, tx *common.VersionedTransaction, finalized bool) error {
	timestamp := s.Timestamp
	if timestamp == 0 && s.NodeId == node.IdForNetwork {
		timestamp = clock.NowUnixNano()
	}
	if s.RoundNumber != 0 {
		return fmt.Errorf("invalid snapshot round %d", s.RoundNumber)
	}

	chain := node.getOrCreateChain(s.NodeId)
	ver, err := chain.buildNodeAcceptTransaction(timestamp, finalized)
	if err != nil {
		return err
	}
	if ver.PayloadHash() != tx.PayloadHash() {
		return fmt.Errorf("invalid node accept transaction %s %s", ver.PayloadHash(), tx.PayloadHash())
	}

	return nil
}

func (node *Node) reloadConsensusState(s *common.Snapshot, tx *common.VersionedTransaction) error {
	if tx.TransactionType() == common.TransactionTypeMint {
		mint := node.lastMintDistribution()
		if mint.Batch < node.LastMint {
			panic(node.LastMint)
		}
		node.LastMint = mint.Batch
		return node.WriteConsensusSnapshotWithHack(s, tx)
	}
	switch tx.TransactionType() {
	case common.TransactionTypeNodePledge,
		common.TransactionTypeNodeCancel,
		common.TransactionTypeNodeAccept,
		common.TransactionTypeNodeRemove,
		common.TransactionTypeCustodianUpdateNodes,
		common.TransactionTypeCustodianSlashNodes:
		err := node.WriteConsensusSnapshotWithHack(s, tx)
		if err != nil {
			return err
		}
	default:
		return nil
	}
	logger.Printf("reloadConsensusState(%v, %v)\n", s, tx)
	err := node.LoadConsensusNodes()
	if err != nil {
		return err
	}

	chain := node.BootChain(s.NodeId)
	err = chain.loadState()
	if err != nil {
		return err
	}
	if chain.ConsensusInfo == nil {
		panic("should never be here")
	}
	switch tx.TransactionType() {
	case common.TransactionTypeNodePledge:
	case common.TransactionTypeNodeAccept:
	case common.TransactionTypeNodeRemove:
	default:
		return nil
	}

	signer := tx.NodeTransactionExtraAsSigner()
	id := signer.Hash().ForNetwork(node.networkId)
	if id == s.NodeId {
		return nil
	}

	chain = node.BootChain(id)
	err = chain.loadState()
	if err != nil {
		return err
	}
	if chain.ConsensusInfo == nil {
		panic("should never be here")
	}
	return nil
}

func (node *Node) finalizeNodeAcceptSnapshot(s *common.Snapshot, signers []crypto.Hash) error {
	logger.Printf("finalizeNodeAcceptSnapshot(%v)\n", s)
	cache := &CacheRound{
		NodeId:    s.NodeId,
		Number:    s.RoundNumber,
		Timestamp: s.Timestamp,
	}
	if err := cache.validateSnapshot(s, true); err != nil {
		panic("should never be here")
	}
	err := node.persistStore.StartNewRound(cache.NodeId, cache.Number, cache.References, 0)
	if err != nil {
		panic(err)
	}

	node.TopoWrite(s, signers)

	final := cache.asFinal()
	external, err := node.getInitialExternalReference(s)
	if err != nil {
		panic(err)
	}
	cache = &CacheRound{
		NodeId:    s.NodeId,
		Number:    1,
		Timestamp: s.Timestamp + config.SnapshotRoundGap + 1,
		References: &common.RoundLink{
			Self:     final.Hash,
			External: external.Hash,
		},
		index: newRoundIndexCache(),
	}
	err = node.persistStore.StartNewRound(cache.NodeId, cache.Number, cache.References, final.Start)
	if err != nil {
		panic(err)
	}

	chain := node.BootChain(s.NodeId)
	err = chain.loadState()
	if err != nil {
		return err
	}
	chain.StepForward()
	chain.assignNewGraphRound(final, cache)
	return nil
}

func (node *Node) getInitialExternalReference(s *common.Snapshot) (*FinalRound, error) {
	nodeDistance := func(a, b crypto.Hash) int {
		ai := new(big.Int).SetBytes(a[:])
		bi := new(big.Int).SetBytes(b[:])
		si := new(big.Int).Sub(ai, bi)
		ai = new(big.Int).Abs(si)
		mi := new(big.Int).Mod(ai, big.NewInt(100))
		return int(mi.Int64())
	}

	externalId := node.genesisNodes[0]
	distance := nodeDistance(s.NodeId, externalId)
	for _, id := range node.genesisNodes {
		nd := nodeDistance(s.NodeId, id)
		if nd < distance {
			distance = nd
			externalId = id
		}
	}

	return loadFinalRoundForNode(node.persistStore, externalId, 0)
}

func (node *Node) validateNodePledgeSnapshot(s *common.Snapshot, tx *common.VersionedTransaction, finalized bool) error {
	timestamp, totalNodes := s.Timestamp, 0
	if s.Timestamp == 0 && s.NodeId == node.IdForNetwork {
		timestamp = clock.NowUnixNano()
	}
	eid := node.electSnapshotNode(common.TransactionTypeNodePledge, timestamp)
	if eid != s.NodeId {
		return fmt.Errorf("node pledge operation at %d only by %s not %s", timestamp, eid, s.NodeId)
	}

	if timestamp < node.Epoch {
		return fmt.Errorf("invalid snapshot timestamp %d %d", node.Epoch, timestamp)
	}
	if !node.checkConsensusPledgeHour(timestamp) {
		hour := (timestamp - node.Epoch) / uint64(time.Hour) % 24
		return fmt.Errorf("invalid node pledge hour %d", hour)
	}
	if tx.Outputs[0].Amount.Cmp(common.KernelNodePledgeAmount) != 0 {
		return fmt.Errorf("invalid pledge amount %s", tx.Outputs[0].Amount.String())
	}

	var signerSpend crypto.Key
	copy(signerSpend[:], tx.Extra)
	offset := timestamp + uint64(config.KernelNodePledgePeriodMinimum)
	for _, cn := range node.NodesListWithoutState(offset, false) {
		if cn.Transaction == tx.PayloadHash() {
			return nil
		}
		if cn.Timestamp > timestamp {
			return fmt.Errorf("invalid snapshot timestamp %d %d", cn.Timestamp, timestamp)
		}
		elapse := time.Duration(timestamp - cn.Timestamp)
		if elapse < config.KernelNodePledgePeriodMinimum {
			return fmt.Errorf("invalid pledge period %d %d", config.KernelNodePledgePeriodMinimum, elapse)
		}
		if cn.Signer.PublicSpendKey.String() == signerSpend.String() {
			return fmt.Errorf("invalid node signer key %s %s", hex.EncodeToString(tx.Extra), cn.Signer)
		}
		if cn.Payee.PublicSpendKey.String() == signerSpend.String() {
			return fmt.Errorf("invalid node signer key %s %s", hex.EncodeToString(tx.Extra), cn.Payee)
		}
		switch cn.State {
		case common.NodeStateAccepted:
			totalNodes = totalNodes + 1
		case common.NodeStateRemoved:
		case common.NodeStateCancelled:
		default:
			return fmt.Errorf("invalid node pending state %s %s", cn.Signer, cn.State)
		}
	}

	if totalNodes >= MaxKernelNodesCount {
		return fmt.Errorf("maximum kernel nodes count reached because cosi signauture mask limit %s", tx.PayloadHash())
	}
	// FIXME the node operation lock threshold should be optimized on pledging period
	return node.persistStore.AddNodeOperation(tx, timestamp, uint64(config.KernelNodePledgePeriodMinimum)*2, finalized)
}

func (node *Node) validateNodeCancelSnapshot(s *common.Snapshot, tx *common.VersionedTransaction, finalized bool) error {
	timestamp := s.Timestamp
	if s.Timestamp == 0 && s.NodeId == node.IdForNetwork {
		timestamp = clock.NowUnixNano()
	}
	if timestamp < node.Epoch {
		return fmt.Errorf("invalid snapshot timestamp %d %d", node.Epoch, timestamp)
	}

	pledging := node.PledgingNode(timestamp)
	if pledging == nil {
		return fmt.Errorf("invalid consensus status")
	}

	if !node.checkConsensusAcceptHour(timestamp) {
		hour := (timestamp - node.Epoch) / uint64(time.Hour) % 24
		return fmt.Errorf("invalid node cancel hour %d", hour)
	}

	threshold := config.SnapshotRoundGap * config.SnapshotReferenceThreshold
	if !finalized && timestamp+threshold*2 < node.GraphTimestamp {
		return fmt.Errorf("invalid snapshot timestamp %d %d", node.GraphTimestamp, timestamp)
	}

	if timestamp < pledging.Timestamp {
		return fmt.Errorf("invalid snapshot timestamp %d %d", pledging.Timestamp, timestamp)
	}
	elapse := time.Duration(timestamp - pledging.Timestamp)
	if elapse < config.KernelNodeAcceptPeriodMinimum {
		return fmt.Errorf("invalid cancel period %d %d", config.KernelNodeAcceptPeriodMinimum, elapse)
	}
	if elapse > config.KernelNodeAcceptPeriodMaximum {
		return fmt.Errorf("invalid cancel period %d %d", config.KernelNodeAcceptPeriodMaximum, elapse)
	}

	// FIXME the node operation lock threshold should be optimized on pledging period
	return node.persistStore.AddNodeOperation(tx, timestamp, uint64(config.KernelNodePledgePeriodMinimum)*2, finalized)
}

func (node *Node) checkConsensusAcceptHour(timestamp uint64) bool {
	hour := (timestamp - node.Epoch) / uint64(time.Hour) % 24
	return hour >= config.KernelNodeAcceptTimeBegin && hour <= config.KernelNodeAcceptTimeEnd
}

func (node *Node) checkConsensusPledgeHour(timestamp uint64) bool {
	hour := (timestamp - node.Epoch) / uint64(time.Hour) % 24
	isMint := hour >= config.KernelMintTimeBegin && hour <= config.KernelMintTimeEnd
	isAccept := hour >= config.KernelNodeAcceptTimeBegin && hour <= config.KernelNodeAcceptTimeEnd
	return !isMint && !isAccept
}
