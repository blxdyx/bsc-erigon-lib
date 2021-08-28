/*
   Copyright 2021 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package txpool

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/btree"
	"github.com/hashicorp/golang-lru/simplelru"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/log/v3"
	"go.uber.org/atomic"
)

const ASSERT = false

type Config struct {
	syncToNewPeersEvery     time.Duration
	commitEvery             time.Duration
	logEvery                time.Duration
	evictSendersAfterRounds uint64
}

var DefaultConfig = Config{
	syncToNewPeersEvery:     2 * time.Minute,
	commitEvery:             15 * time.Second,
	logEvery:                30 * time.Second,
	evictSendersAfterRounds: 10,
}

// Pool is interface for the transaction pool
// This interface exists for the convinience of testing, and not yet because
// there are multiple implementations
type Pool interface {
	// IdHashKnown check whether transaction with given Id hash is known to the pool
	IdHashKnown(tx kv.Tx, hash []byte) (bool, error)
	Started() bool
	GetRlp(tx kv.Tx, hash []byte) ([]byte, error)
	OnNewTxs(ctx context.Context, db kv.RoDB, newTxs TxSlots, senders *SendersCache) error
	OnNewBlock(stateChanges map[string]senderInfo, unwindTxs, minedTxs TxSlots, protocolBaseFee, pendingBaseFee, blockHeight uint64, blockHash [32]byte, senders *SendersCache) error

	AddNewGoodPeer(peerID PeerID)
}

var _ Pool = (*TxPool)(nil) // compile-time interface check

// SubPoolMarker ordered bitset responsible to sort transactions by sub-pools. Bits meaning:
// 1. Minimum fee requirement. Set to 1 if feeCap of the transaction is no less than in-protocol parameter of minimal base fee. Set to 0 if feeCap is less than minimum base fee, which means this transaction will never be included into this particular chain.
// 2. Absence of nonce gaps. Set to 1 for transactions whose nonce is N, state nonce for the sender is M, and there are transactions for all nonces between M and N from the same sender. Set to 0 is the transaction's nonce is divided from the state nonce by one or more nonce gaps.
// 3. Sufficient balance for gas. Set to 1 if the balance of sender's account in the state is B, nonce of the sender in the state is M, nonce of the transaction is N, and the sum of feeCap x gasLimit + transferred_value of all transactions from this sender with nonces N+1 ... M is no more than B. Set to 0 otherwise. In other words, this bit is set if there is currently a guarantee that the transaction and all its required prior transactions will be able to pay for gas.
// 4. Dynamic fee requirement. Set to 1 if feeCap of the transaction is no less than baseFee of the currently pending block. Set to 0 otherwise.
// 5. Local transaction. Set to 1 if transaction is local.
type SubPoolMarker uint8

const (
	EnoughFeeCapProtocol = 0b10000
	NoNonceGaps          = 0b01000
	EnoughBalance        = 0b00100
	EnoughFeeCapBlock    = 0b00010
	IsLocal              = 0b00001
)

// metaTx holds transaction and some metadata
type metaTx struct {
	subPool        SubPoolMarker
	effectiveTip   uint64 // max(minTip, minFeeCap - baseFee)
	Tx             *TxSlot
	bestIndex      int
	worstIndex     int
	currentSubPool SubPoolType
}

func newMetaTx(slot *TxSlot, isLocal bool) *metaTx {
	mt := &metaTx{Tx: slot, worstIndex: -1, bestIndex: -1}
	if isLocal {
		mt.subPool = IsLocal
	}
	return mt
}

type SubPoolType uint8

const PendingSubPool SubPoolType = 1
const BaseFeeSubPool SubPoolType = 2
const QueuedSubPool SubPoolType = 3

const PendingSubPoolLimit = 10 * 1024
const BaseFeeSubPoolLimit = 10 * 1024
const QueuedSubPoolLimit = 10 * 1024

const MaxSendersInfoCache = 2 * (PendingSubPoolLimit + BaseFeeSubPoolLimit + QueuedSubPoolLimit)

type senderInfo struct {
	balance uint256.Int
	nonce   uint64
}

//nolint
func newSenderInfo(nonce uint64, balance uint256.Int) *senderInfo {
	return &senderInfo{nonce: nonce, balance: balance}
}

type sortByNonce struct{ *metaTx }

func (i *sortByNonce) Less(than btree.Item) bool {
	if i.metaTx.Tx.senderID != than.(*sortByNonce).metaTx.Tx.senderID {
		return i.metaTx.Tx.senderID < than.(*sortByNonce).metaTx.Tx.senderID
	}
	return i.metaTx.Tx.nonce < than.(*sortByNonce).metaTx.Tx.nonce
}

type SendersCache struct {
	lock        sync.RWMutex
	blockHeight atomic.Uint64
	blockHash   atomic.String
	senderID    uint64
	commitID    uint64
	senderIDs   map[string]uint64
	senderInfo  map[uint64]*senderInfo
}

func NewSendersCache() *SendersCache {
	return &SendersCache{
		senderIDs:  map[string]uint64{},
		senderInfo: map[uint64]*senderInfo{},
	}
}

//nolint
func (sc *SendersCache) idsCount(tx kv.Tx) (inMem int, inDb int, err error) {
	c, err := tx.Cursor(kv.PooledSenderID)
	if err != nil {
		return 0, 0, err
	}
	inDB, err := c.Count()
	if err != nil {
		return 0, 0, err
	}
	return len(sc.senderIDs), int(inDB), nil
}

//nolint
func (sc *SendersCache) infoCount(tx kv.Tx) (inMem int, inDb int, err error) {
	c, err := tx.Cursor(kv.PooledSender)
	if err != nil {
		return 0, 0, err
	}
	inDB, err := c.Count()
	if err != nil {
		return 0, 0, err
	}
	return len(sc.senderInfo), int(inDB), nil
}
func (sc *SendersCache) Id(addr string, tx kv.Tx) (uint64, bool, error) {
	sc.lock.RLock() //TODO: it may load mapping from db,maybe just use thread-safe maps to don't worry much
	defer sc.lock.RUnlock()
	return sc.id(addr, tx)
}
func (sc *SendersCache) id(addr string, tx kv.Tx) (uint64, bool, error) {
	id, ok := sc.senderIDs[addr]
	if !ok {
		v, err := tx.GetOne(kv.PooledSenderID, []byte(addr))
		if err != nil {
			return 0, false, err
		}
		if len(v) == 0 {
			return 0, false, nil
		}
		id = binary.BigEndian.Uint64(v)
	}
	return id, true, nil
}
func (sc *SendersCache) Info(id uint64, tx kv.Tx, expectMiss bool) (*senderInfo, error) {
	sc.lock.RLock()
	defer sc.lock.RUnlock()
	return sc.info(id, tx, expectMiss)
}
func (sc *SendersCache) info(id uint64, tx kv.Tx, expectMiss bool) (*senderInfo, error) {
	info, ok := sc.senderInfo[id]
	if !ok {
		encID := make([]byte, 8)
		binary.BigEndian.PutUint64(encID, id)
		v, err := tx.GetOne(kv.PooledSender, encID)
		if err != nil {
			return nil, err
		}
		if len(v) == 0 {
			if !expectMiss {
				fmt.Printf("sender not loaded in advance: %d\n", id)
				panic("all senders must be loaded in advance")
			}
			return nil, nil // don't fallback to core db, it will be manually done in right place
		}
		balance := uint256.NewInt(0)
		balance.SetBytes(v[8:])
		info = newSenderInfo(binary.BigEndian.Uint64(v), *balance)
	}
	return info, nil
}

//nolint
func (sc *SendersCache) printDebug(prefix string) {
	fmt.Printf("%s.SendersCache.senderInfo\n", prefix)
	for i, j := range sc.senderInfo {
		fmt.Printf("\tid=%d,nonce=%d,balance=%d\n", i, j.nonce, j.balance.Uint64())
	}
}

func (sc *SendersCache) onNewTxs(tx kv.Tx, newTxs TxSlots) (cacheMisses map[uint64]string, err error) {
	if err := sc.ensureSenderIDOnNewTxs(tx, newTxs); err != nil {
		return nil, err
	}
	cacheMisses, err = sc.setTxSenderID(tx, newTxs)
	if err != nil {
		return nil, err
	}
	return cacheMisses, nil
}
func (sc *SendersCache) loadFromCore(coreTx kv.Tx, toLoad map[uint64]string) error {
	diff := make(map[uint64]*senderInfo, len(toLoad))
	for id := range toLoad {
		info, err := loadSender(coreTx, []byte(toLoad[id]))
		if err != nil {
			return err
		}
		diff[id] = info
	}
	sc.lock.Lock()
	defer sc.lock.Unlock()
	for id := range diff { // merge state changes
		a := diff[id]
		sc.senderInfo[id] = a
	}
	return nil
}

func (sc *SendersCache) onNewBlock(tx kv.Tx, stateChanges map[string]senderInfo, unwindTxs, minedTxs TxSlots, blockHeight uint64, blockHash [32]byte) error {
	//TODO: if see non-continuous block heigh - load gap from changesets
	sc.blockHeight.Store(blockHeight)
	sc.blockHash.Store(string(blockHash[:]))

	//`loadSenders` goes by network to core - and it must be outside of SendersCache lock. But other methods must be locked
	if err := sc.mergeStateChanges(tx, stateChanges, unwindTxs, minedTxs); err != nil {
		return err
	}
	if _, err := sc.setTxSenderID(tx, unwindTxs); err != nil {
		return err
	}
	if _, err := sc.setTxSenderID(tx, minedTxs); err != nil {
		return err
	}
	return nil
}
func (sc *SendersCache) mergeStateChanges(tx kv.Tx, stateChanges map[string]senderInfo, unwindedTxs, minedTxs TxSlots) error {
	sc.lock.Lock()
	defer sc.lock.Unlock()
	return sc.mergeStateChangesLocked(tx, stateChanges, unwindedTxs, minedTxs)
}

func (sc *SendersCache) mergeStateChangesLocked(tx kv.Tx, stateChanges map[string]senderInfo, unwindedTxs, minedTxs TxSlots) error {
	for addr, v := range stateChanges { // merge state changes
		id, ok, err := sc.id(addr, tx)
		if err != nil {
			return err
		}
		if !ok {
			sc.senderID++
			id = sc.senderID
			sc.senderIDs[addr] = id
		}
		sc.senderInfo[id] = newSenderInfo(v.nonce, v.balance)
	}

	/*
		for addr, id := range sc.senderIDs { // merge state changes
			if v, ok := stateChanges[addr]; ok {
				sc.senderInfo[id] = newSenderInfo(v.nonce, v.balance)
			}
		}
	*/

	for i := 0; i < unwindedTxs.senders.Len(); i++ {
		id, ok, err := sc.id(string(unwindedTxs.senders.At(i)), tx)
		if err != nil {
			return err
		}
		if !ok {
			sc.senderID++
			id = sc.senderID
			sc.senderIDs[string(unwindedTxs.senders.At(i))] = id
		}
		if _, ok := sc.senderInfo[id]; !ok {
			if _, ok := stateChanges[string(unwindedTxs.senders.At(i))]; !ok {
				sc.senderInfo[id] = newSenderInfo(0, *uint256.NewInt(0))
			}
		}
	}

	for i := 0; i < len(minedTxs.txs); i++ {
		id, ok, err := sc.id(string(minedTxs.senders.At(i)), tx)
		if err != nil {
			return err
		}
		if !ok {
			sc.senderID++
			id = sc.senderID
			sc.senderIDs[string(minedTxs.senders.At(i))] = id
		}
		if _, ok := sc.senderInfo[id]; !ok {
			if _, ok := stateChanges[string(minedTxs.senders.At(i))]; !ok {
				sc.senderInfo[id] = newSenderInfo(0, *uint256.NewInt(0))
			}
		}

		//if v, ok := stateChanges[string(minedTxs.senders.At(i))]; ok {
		//	sc.senderInfo[id] = newSenderInfo(v.nonce, v.balance)
		//}
	}
	return nil
}

func (sc *SendersCache) ensureSenderIDOnNewTxs(tx kv.Tx, newTxs TxSlots) error {
	sc.lock.Lock()
	defer sc.lock.Unlock()
	for i := 0; i < len(newTxs.txs); i++ {
		_, ok, err := sc.id(string(newTxs.senders.At(i)), tx)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		sc.senderID++
		sc.senderIDs[string(newTxs.senders.At(i))] = sc.senderID
	}
	return nil
}

func (sc *SendersCache) setTxSenderID(tx kv.Tx, txs TxSlots) (map[uint64]string, error) {
	sc.lock.RLock()
	defer sc.lock.RUnlock()
	toLoad := map[uint64]string{}
	for i := range txs.txs {
		addr := string(txs.senders.At(i))

		// assign ID to each new sender
		id, ok, err := sc.id(addr, tx)
		if err != nil {
			return nil, err
		}
		if !ok {
			panic("not supported yet")
		}
		txs.txs[i].senderID = id

		// load data from db if need
		info, err := sc.info(txs.txs[i].senderID, tx, true)
		if err != nil {
			return nil, err
		}
		if info != nil {
			continue
		}
		_, ok = toLoad[txs.txs[i].senderID]
		if ok {
			continue
		}
		toLoad[txs.txs[i].senderID] = addr
	}
	return toLoad, nil
}
func (sc *SendersCache) fromDB(ctx context.Context, tx kv.RwTx, coreTx kv.Tx) error {
	sc.lock.Lock()
	defer sc.lock.Unlock()

	{
		v, err := tx.GetOne(kv.PoolInfo, SenderCacheHeightKey)
		if err != nil {
			return err
		}
		if len(v) > 0 {
			sc.blockHeight.Store(binary.BigEndian.Uint64(v))
		}
	}
	{
		v, err := tx.GetOne(kv.PoolInfo, SenderCacheHashKey)
		if err != nil {
			return err
		}
		if len(v) > 0 {
			sc.blockHash.Store(string(v))
		}
	}
	{
		v, err := tx.GetOne(kv.PoolInfo, SenderCacheIDKey)
		if err != nil {
			return err
		}
		if len(v) > 0 {
			sc.senderID = binary.BigEndian.Uint64(v)
		}
	}
	{
		v, err := tx.GetOne(kv.PoolInfo, SenderCommitIDKey)
		if err != nil {
			return err
		}
		if len(v) > 0 {
			sc.commitID = binary.BigEndian.Uint64(v)
		}
	}

	if err := sc.syncMissedStateDiff(ctx, tx, coreTx, 0); err != nil {
		return err
	}
	return nil
}
func (sc *SendersCache) syncMissedStateDiff(ctx context.Context, tx kv.RwTx, coreTx kv.Tx, missedTo uint64) error {
	dropLocalSendersCache := false
	if missedTo > 0 && missedTo-sc.blockHeight.Load() > 1024 {
		dropLocalSendersCache = true
	}
	lastCommitTimeV, err := tx.GetOne(kv.PoolInfo, SenderCommitTimeKey)
	if err != nil {
		return err
	}
	lastCommitTime := time.Time{}
	if len(lastCommitTimeV) > 0 {
		if err := lastCommitTime.UnmarshalBinary(lastCommitTimeV); err != nil {
			return err
		}
		if time.Since(lastCommitTime) > 3*24*time.Hour {
			dropLocalSendersCache = true
		}
	}
	if coreTx != nil {
		ok, err := isCanonical(coreTx, sc.blockHeight.Load(), []byte(sc.blockHash.Load()))
		if err != nil {
			return err
		}
		if !ok {
			dropLocalSendersCache = true
		}
	}

	if dropLocalSendersCache {
		if err := tx.ClearBucket(kv.PooledSender); err != nil {
			return err
		}
		sc.senderInfo = map[uint64]*senderInfo{}
	}

	if missedTo == 0 {
		missedTo = sc.blockHeight.Load()
		if missedTo == 0 {
			return nil
		}
	}

	if coreTx == nil {
		return nil
	}
	diff, err := changesets(ctx, sc.blockHeight.Load(), coreTx)
	if err != nil {
		return err
	}
	if err := sc.mergeStateChangesLocked(tx, diff, TxSlots{}, TxSlots{}); err != nil {
		return err
	}
	return nil
}
func isCanonical(coreTx kv.Tx, num uint64, hash []byte) (bool, error) {
	encNum := make([]byte, 8)
	binary.BigEndian.PutUint64(encNum, num)
	canonical, err := coreTx.GetOne(kv.HeaderCanonical, encNum)
	if err != nil {
		return false, err
	}

	return bytes.Equal(hash, canonical), nil
}

func changesets(ctx context.Context, from uint64, coreTx kv.Tx) (map[string]senderInfo, error) {
	encNum := make([]byte, 8)
	diff := map[string]senderInfo{}
	binary.BigEndian.PutUint64(encNum, from)
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	//TODO: tx.ForEach must be implemented as buffered server-side stream
	if err := coreTx.ForEach(kv.AccountChangeSet, encNum, func(k, v []byte) error {
		info, err := loadSender(coreTx, v[:20])
		if err != nil {
			return err
		}
		diff[string(v[:20])] = *info
		select {
		case <-logEvery.C:
			log.Info("loading changesets", "block", binary.BigEndian.Uint64(k))
		case <-ctx.Done():
			return nil
		default:
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return diff, nil
}

var SenderCommitTimeKey = []byte("sender_commit_time")
var SenderCacheIDKey = []byte("sender_cache_id")
var SenderCommitIDKey = []byte("sender_commit_id")
var SenderCacheHeightKey = []byte("sender_cache_block_height")
var SenderCacheHashKey = []byte("sender_cache_block_hash")
var PoolPendingBaseFeeKey = []byte("pending_base_fee")
var PoolProtocolBaseFeeKey = []byte("protocol_base_fee")

func (sc *SendersCache) flush(tx kv.RwTx, byNonce *ByNonce, sendersWithoutTransactions []uint64, evictAfterRounds uint64) (evicted uint64, err error) {
	sc.lock.Lock()
	defer sc.lock.Unlock()
	sc.commitID++

	encID := make([]byte, 8)
	encIDs := make([]byte, 0, 8*len(sendersWithoutTransactions))
	// Eviction logic. Store into db list of senders:
	//      - which have discarded transactions at this commit
	//      - but have no active transactions left
	// after some time read old records from DB and if such senders still have no transactions - evict them
	for _, id := range sendersWithoutTransactions {
		binary.BigEndian.PutUint64(encID, id)
		encIDs = append(encIDs, encID...)
		binary.BigEndian.PutUint64(encID, sc.commitID)
	}
	if len(encIDs) > 0 {
		if err := tx.Append(kv.PoolStateEviction, encID, encIDs); err != nil {
			return evicted, err
		}
	}

	c, err := tx.RwCursor(kv.PoolStateEviction)
	if err != nil {
		return evicted, err
	}
	defer c.Close()
	//justDeleted := []uint64{}
	for k, v, err := c.First(); k != nil; k, v, err = c.Next() {
		if err != nil {
			return evicted, err
		}
		if sc.commitID-binary.BigEndian.Uint64(k) < evictAfterRounds {
			break
		}
		for i := 0; i < len(v); i += 8 {
			senderID := binary.BigEndian.Uint64(v[i : i+8])
			if _, ok := sc.senderInfo[senderID]; ok {
				continue
			}
			if byNonce.count(senderID) > 0 {
				continue
			}
			addr, err := tx.GetOne(kv.PooledSenderID, encID)
			if err != nil {
				return evicted, err
			}
			if _, ok := sc.senderIDs[string(addr)]; ok {
				continue
			}
			if err := tx.Delete(kv.PooledSenderID, addr, nil); err != nil {
				return evicted, err
			}
			if err := tx.Delete(kv.PooledSenderIDToAdress, encID, nil); err != nil {
				return evicted, err
			}
			if err := tx.Delete(kv.PooledSender, encID, nil); err != nil {
				return evicted, err
			}
			evicted++
			//justDeleted = append(justDeleted, senderID)
		}
		if err := c.DeleteCurrent(); err != nil {
			return evicted, err
		}
	}

	//justInserted := []uint64{}
	for addr, id := range sc.senderIDs {
		binary.BigEndian.PutUint64(encID, id)
		currentV, err := tx.GetOne(kv.PooledSenderID, []byte(addr))
		if err != nil {
			return evicted, err
		}
		if currentV != nil && bytes.Equal(currentV, encID) {
			continue
		}
		//fmt.Printf("Put: %d\n", id)
		if err := tx.Put(kv.PooledSenderID, []byte(addr), encID); err != nil {
			return evicted, err
		}
		if err := tx.Put(kv.PooledSenderIDToAdress, encID, []byte(addr)); err != nil {
			return evicted, err
		}
		//justInserted = append(justInserted, id)
	}
	//sort.Slice(justInserted, func(i, j int) bool { return justInserted[i] < justInserted[j] })

	v := make([]byte, 8, 8+32)
	for id, info := range sc.senderInfo {
		binary.BigEndian.PutUint64(encID, id)
		binary.BigEndian.PutUint64(v, info.nonce)
		v = append(v[:8], info.balance.Bytes()...)
		if err := tx.Put(kv.PooledSender, encID, v); err != nil {
			return evicted, err
		}
	}
	//fmt.Printf("justDeleted:%d, justInserted:%d\n", justDeleted, justInserted)
	if ASSERT {
		{
			duplicates := map[string]uint64{}
			_ = tx.ForPrefix(kv.PooledSenderIDToAdress, nil, func(k, v []byte) error {
				id, ok := duplicates[string(v)]
				if ok {
					fmt.Printf("duplicate: %d,%d,%x\n", id, binary.BigEndian.Uint64(k), string(v))
					panic(1)
				}
				return nil
			})
		}
		{
			duplicates := map[uint64]string{}
			_ = tx.ForPrefix(kv.PooledSenderIDToAdress, nil, func(k, v []byte) error {
				id := binary.BigEndian.Uint64(v)
				addr, ok := duplicates[id]
				if ok {
					fmt.Printf("duplicate: %x,%x,%d\n", addr, k, binary.BigEndian.Uint64(v))
					panic(1)
				}
				return nil
			})
		}
	}
	if ASSERT {
		_ = tx.ForEach(kv.PooledTransaction, nil, func(k, v []byte) error {
			//id := binary.BigEndian.Uint64(v[:8])
			//for _, senderID := range justDeleted {
			//	if senderID == id {
			//		fmt.Printf("delted id still has tx in db: %d,%x\n", id, k)
			//		panic(1)
			//	}
			//}
			vv, err := tx.GetOne(kv.PooledSenderIDToAdress, v[:8])
			if err != nil {
				return err
			}
			if len(vv) == 0 {
				cc, _ := tx.Cursor(kv.PooledSenderIDToAdress)
				last, lastAddr, _ := cc.Last()
				slots := TxSlots{}
				slots.Growth(1)
				slots.txs[0] = &TxSlot{}
				parseCtx := NewTxParseContext()
				_, err := parseCtx.ParseTransaction(v[8+8:], 0, slots.txs[0], slots.senders.At(0))
				if err != nil {
					log.Error("er", "er", err)
				}
				fmt.Printf("sender:%x, txHash: %x\n", slots.senders.At(0), slots.txs[0].idHash)

				fmt.Printf("last: %d,%x\n", binary.BigEndian.Uint64(last), lastAddr)
				fmt.Printf("now: %d\n", sc.senderID)
				fmt.Printf("not foundd: %d,%x,%x,%x\n", binary.BigEndian.Uint64(v[:8]), k, v, vv)
				fmt.Printf("aa: %x,%x,%x\n", k, v, vv)
				//fmt.Printf("justDeleted:%d, justInserted:%d\n", justDeleted, justInserted)
				panic("no-no")
			}
			return nil
		})
	}

	binary.BigEndian.PutUint64(encID, sc.blockHeight.Load())
	if err := tx.Put(kv.PoolInfo, SenderCacheHeightKey, encID); err != nil {
		return evicted, err
	}
	if err := tx.Put(kv.PoolInfo, SenderCacheHashKey, []byte(sc.blockHash.Load())); err != nil {
		return evicted, err
	}
	binary.BigEndian.PutUint64(encID, sc.senderID)
	if err := tx.Put(kv.PoolInfo, SenderCacheIDKey, encID); err != nil {
		return evicted, err
	}
	binary.BigEndian.PutUint64(encID, sc.commitID)
	if err := tx.Put(kv.PoolInfo, SenderCommitIDKey, encID); err != nil {
		return evicted, err
	}
	lastCommitTime, err := time.Now().MarshalBinary()
	if err != nil {
		return evicted, err
	}
	if err := tx.Put(kv.PoolInfo, SenderCommitTimeKey, lastCommitTime); err != nil {
		return evicted, err
	}

	sc.senderIDs = map[string]uint64{}
	sc.senderInfo = map[uint64]*senderInfo{}
	return evicted, nil
}

func loadSender(coreTx kv.Tx, addr []byte) (*senderInfo, error) {
	encoded, err := coreTx.GetOne(kv.PlainState, addr)
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 {
		//return nil, nil
		return newSenderInfo(0, *uint256.NewInt(0)), nil
	}
	nonce, balance, err := DecodeSender(encoded)
	if err != nil {
		return nil, err
	}
	return newSenderInfo(nonce, balance), nil
}

// TxPool - holds all pool-related data structures and lock-based tiny methods
// most of logic implemented by pure tests-friendly functions
type TxPool struct {
	lock *sync.RWMutex

	protocolBaseFee atomic.Uint64
	pendingBaseFee  atomic.Uint64

	senderID                 uint64
	byHash                   map[string]*metaTx // tx_hash => tx
	pending, baseFee, queued *SubPool

	// track isLocal flag of already mined transactions. used at unwind.
	localsHistory *simplelru.LRU
	db            kv.RwDB

	// fields for transaction propagation
	recentlyConnectedPeers *recentlyConnectedPeers
	newTxs                 chan Hashes
	deletedTxs             []*metaTx
	senders                *SendersCache
	txNonce2Tx             *ByNonce // senderID => (sorted map of tx nonce => *metaTx)
	cfg                    Config
}

type ByNonce struct {
	tree *btree.BTree
}

func (b *ByNonce) ascend(senderID uint64, f func(tx *metaTx) bool) {
	b.tree.AscendGreaterOrEqual(&sortByNonce{&metaTx{Tx: &TxSlot{senderID: senderID}}}, func(i btree.Item) bool {
		mt := i.(*sortByNonce).metaTx
		if mt.Tx.senderID != senderID {
			return false
		}
		return f(mt)
	})
}
func (b *ByNonce) count(senderID uint64) (count uint64) {
	b.ascend(senderID, func(tx *metaTx) bool {
		count++
		return true
	})
	return count
}
func (b *ByNonce) get(senderID, txNonce uint64) *metaTx {
	if found := b.tree.Get(&sortByNonce{&metaTx{Tx: &TxSlot{senderID: senderID, nonce: txNonce}}}); found != nil {
		return found.(*sortByNonce).metaTx
	}
	return nil
}

//nolint
func (b *ByNonce) has(mt *metaTx) bool {
	found := b.tree.Get(&sortByNonce{mt})
	return found != nil
}
func (b *ByNonce) delete(mt *metaTx) {
	deleted := b.tree.Delete(&sortByNonce{mt})
	if ASSERT {
		if deleted != nil && deleted.(*sortByNonce).metaTx.Tx.idHash != mt.Tx.idHash {
			panic(1)
		}
	}
}
func (b *ByNonce) replaceOrInsert(mt *metaTx) *metaTx {
	if ASSERT && (mt == nil || mt.Tx == nil || mt.Tx.senderID == 0) {
		panic("must never happen")
	}
	it := b.tree.ReplaceOrInsert(&sortByNonce{mt})
	if it != nil {
		return it.(*sortByNonce).metaTx
	}
	return nil
}

func New(newTxs chan Hashes, senders *SendersCache, db kv.RwDB, cfg Config) (*TxPool, error) {
	localsHistory, err := simplelru.NewLRU(1024, nil)
	if err != nil {
		return nil, err
	}
	return &TxPool{
		lock:                   &sync.RWMutex{},
		byHash:                 map[string]*metaTx{},
		txNonce2Tx:             &ByNonce{btree.New(32)},
		localsHistory:          localsHistory,
		recentlyConnectedPeers: &recentlyConnectedPeers{},
		pending:                NewSubPool(PendingSubPool),
		baseFee:                NewSubPool(BaseFeeSubPool),
		queued:                 NewSubPool(QueuedSubPool),
		newTxs:                 newTxs,
		senders:                senders,
		db:                     db,
		cfg:                    cfg,
		senderID:               1,
	}, nil
}

//nolint
func (p *TxPool) printDebug(prefix string) {
	fmt.Printf("%s.pool.byHash\n", prefix)
	for _, j := range p.byHash {
		fmt.Printf("\tsenderID=%d, nonce=%d, tip=%d\n", j.Tx.senderID, j.Tx.nonce, j.Tx.tip)
	}
	fmt.Printf("%s.pool.queues.len: %d,%d,%d\n", prefix, p.pending.Len(), p.baseFee.Len(), p.queued.Len())
	for i := range *p.pending.best {
		(*p.pending.best)[i].Tx.printDebug(fmt.Sprintf("%s.pending: %b", prefix, (*p.pending.best)[i].subPool))
	}
	for i := range *p.queued.best {
		(*p.queued.best)[i].Tx.printDebug(fmt.Sprintf("%s.queued : %b", prefix, (*p.queued.best)[i].subPool))
	}
}
func (p *TxPool) logStats(tx kv.Tx) error {
	protocolBaseFee, pendingBaseFee := p.protocolBaseFee.Load(), p.pendingBaseFee.Load()

	p.lock.RLock()
	defer p.lock.RUnlock()
	idsInMem, idsInDb, err := p.senders.idsCount(tx)
	if err != nil {
		return err
	}
	infoInMem, infoInDb, err := p.senders.infoCount(tx)
	if err != nil {
		return err
	}
	log.Info(fmt.Sprintf("baseFee: %dm->%dm; queuesSize: pending=%d/%d, baseFee=%d/%d, queued=%d/%d; sendersCache: id=%d+%d,info=%d+%d",
		protocolBaseFee/1_000_000, pendingBaseFee/1_000_000,
		p.pending.Len(), PendingSubPoolLimit, p.baseFee.Len(), BaseFeeSubPoolLimit, p.queued.Len(), QueuedSubPoolLimit,
		idsInMem, idsInDb, infoInMem, infoInDb,
	))
	return nil
}
func (p *TxPool) GetRlp(tx kv.Tx, hash []byte) ([]byte, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	txn, ok := p.byHash[string(hash)]
	if !ok || txn.Tx.rlp == nil {
		return tx.GetOne(kv.PooledTransaction, hash)
	}
	return txn.Tx.rlp, nil
}
func (p *TxPool) AppendLocalHashes(buf []byte) []byte {
	p.lock.RLock()
	defer p.lock.RUnlock()
	for hash, txn := range p.byHash {
		if txn.subPool&IsLocal == 0 {
			continue
		}
		buf = append(buf, hash...)
	}
	return buf
}
func (p *TxPool) AppendRemoteHashes(buf []byte) []byte {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for hash, txn := range p.byHash {
		if txn.subPool&IsLocal != 0 {
			continue
		}
		buf = append(buf, hash...)
	}
	return buf
}
func (p *TxPool) AppendAllHashes(buf []byte) []byte {
	buf = p.AppendLocalHashes(buf)
	buf = p.AppendRemoteHashes(buf)
	return buf
}
func (p *TxPool) IdHashKnown(tx kv.Tx, hash []byte) (bool, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	_, ok := p.byHash[string(hash)]
	if !ok {
		return tx.Has(kv.PooledTransaction, hash)
	}
	return true, nil
}
func (p *TxPool) IdHashIsLocal(hash []byte) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	txn, ok := p.byHash[string(hash)]
	if !ok {
		return false
	}
	return txn.subPool&IsLocal != 0
}
func (p *TxPool) AddNewGoodPeer(peerID PeerID) { p.recentlyConnectedPeers.AddPeer(peerID) }
func (p *TxPool) Started() bool                { return p.protocolBaseFee.Load() > 0 }

func (p *TxPool) OnNewTxs(ctx context.Context, coreDB kv.RoDB, newTxs TxSlots, senders *SendersCache) error {
	//t := time.Now()
	p.lock.Lock()
	defer p.lock.Unlock()

	tx, err := p.db.BeginRo(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cacheMisses, err := senders.onNewTxs(tx, newTxs)
	if err != nil {
		return err
	}
	if len(cacheMisses) > 0 {
		if err := coreDB.View(ctx, func(tx kv.Tx) error { return senders.loadFromCore(tx, cacheMisses) }); err != nil {
			return err
		}
	}
	if err := newTxs.Valid(); err != nil {
		return err
	}

	protocolBaseFee, pendingBaseFee := p.protocolBaseFee.Load(), p.pendingBaseFee.Load()
	if protocolBaseFee == 0 || pendingBaseFee == 0 {
		return fmt.Errorf("non-zero base fee: %d,%d", protocolBaseFee, pendingBaseFee)
	}
	if err := onNewTxs(tx, senders, newTxs, protocolBaseFee, pendingBaseFee, p.pending, p.baseFee, p.queued, p.txNonce2Tx, p.byHash, p.discardLocked); err != nil {
		return err
	}
	notifyNewTxs := make(Hashes, 0, 32*len(newTxs.txs))
	for i := range newTxs.txs {
		_, ok := p.byHash[string(newTxs.txs[i].idHash[:])]
		if !ok {
			continue
		}
		notifyNewTxs = append(notifyNewTxs, newTxs.txs[i].idHash[:]...)
	}
	if len(notifyNewTxs) > 0 {
		select {
		case p.newTxs <- notifyNewTxs:
		default:
		}
	}

	//log.Info("on new txs", "in", time.Since(t))
	return nil
}
func onNewTxs(tx kv.Tx, senders *SendersCache, newTxs TxSlots, protocolBaseFee, pendingBaseFee uint64, pending, baseFee, queued *SubPool, byNonce *ByNonce, byHash map[string]*metaTx, discard func(*metaTx)) error {
	for i := range newTxs.txs {
		if newTxs.txs[i].senderID == 0 {
			return fmt.Errorf("senderID can't be zero")
		}
	}

	changedSenders := unsafeAddToPendingPool(byNonce, newTxs, pending, baseFee, queued, byHash, discard)
	for id := range changedSenders {
		sender, err := senders.Info(id, tx, false)
		if err != nil {
			return err
		}
		onSenderChange(id, sender, byNonce, protocolBaseFee, pendingBaseFee)
	}

	pending.EnforceInvariants()
	baseFee.EnforceInvariants()
	queued.EnforceInvariants()

	promote(pending, baseFee, queued, discard)

	return nil
}

func (p *TxPool) setBaseFee(protocolBaseFee, pendingBaseFee uint64) (uint64, uint64) {
	p.protocolBaseFee.Store(protocolBaseFee)
	hasNewVal := pendingBaseFee > 0
	if pendingBaseFee < protocolBaseFee {
		pendingBaseFee = protocolBaseFee
		hasNewVal = true
	}
	if hasNewVal {
		p.pendingBaseFee.Store(pendingBaseFee)
	}
	return protocolBaseFee, p.pendingBaseFee.Load()
}

func (p *TxPool) OnNewBlock(stateChanges map[string]senderInfo, unwindTxs, minedTxs TxSlots, protocolBaseFee, pendingBaseFee, blockHeight uint64, blockHash [32]byte, senders *SendersCache) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	t := time.Now()
	tx, err := p.db.BeginRo(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	protocolBaseFee, pendingBaseFee = p.setBaseFee(protocolBaseFee, pendingBaseFee)
	if err := senders.onNewBlock(tx, stateChanges, unwindTxs, minedTxs, blockHeight, blockHash); err != nil {
		return err
	}
	//log.Debug("[txpool] new block", "unwinded", len(unwindTxs.txs), "mined", len(minedTxs.txs), "protocolBaseFee", protocolBaseFee, "blockHeight", blockHeight)
	if err := unwindTxs.Valid(); err != nil {
		return err
	}
	if err := minedTxs.Valid(); err != nil {
		return err
	}

	if err := onNewBlock(tx, senders, unwindTxs, minedTxs.txs, protocolBaseFee, pendingBaseFee, p.pending, p.baseFee, p.queued, p.txNonce2Tx, p.byHash, p.discardLocked); err != nil {
		return err
	}

	notifyNewTxs := make(Hashes, 0, 32*len(unwindTxs.txs))
	for i := range unwindTxs.txs {
		_, ok := p.byHash[string(unwindTxs.txs[i].idHash[:])]
		if !ok {
			continue
		}
		notifyNewTxs = append(notifyNewTxs, unwindTxs.txs[i].idHash[:]...)
	}
	if len(notifyNewTxs) > 0 {
		select {
		case p.newTxs <- notifyNewTxs:
		default:
		}
	}

	log.Info("on new block", "in", time.Since(t))
	return nil
}
func (p *TxPool) flush(db kv.RwDB) (evicted, written uint64, err error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	//it's important that write db tx is done inside lock, to make last writes visible for all read operations
	if err := db.Update(context.Background(), func(tx kv.RwTx) error {
		evicted, err = p.flushLocked(tx)
		if err != nil {
			return err
		}
		written, _, err = tx.(*mdbx.MdbxTx).SpaceDirty()
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return 0, 0, err
	}
	return evicted, written, nil
}
func (p *TxPool) flushLocked(tx kv.RwTx) (evicted uint64, err error) {
	sendersWithoutTransactionsUnique := map[uint64]struct{}{}
	sendersWithoutTransactions := []uint64{}
	for i := 0; i < len(p.deletedTxs); i++ {
		if p.txNonce2Tx.count(p.deletedTxs[i].Tx.senderID) == 0 {
			sendersWithoutTransactionsUnique[p.deletedTxs[i].Tx.senderID] = struct{}{}
			sendersWithoutTransactions = append(sendersWithoutTransactions, p.deletedTxs[i].Tx.senderID)
		}
		if err := tx.Delete(kv.PooledTransaction, p.deletedTxs[i].Tx.idHash[:], nil); err != nil {
			return evicted, err
		}
		p.deletedTxs[i] = nil // for gc
	}
	sort.Slice(sendersWithoutTransactions, func(i, j int) bool {
		return sendersWithoutTransactions[i] < sendersWithoutTransactions[j]
	})

	txHashes := p.localsHistory.Keys()
	encID := make([]byte, 8)
	if err := tx.ClearBucket(kv.RecentLocalTransaction); err != nil {
		return evicted, err
	}
	for i := range txHashes {
		binary.BigEndian.PutUint64(encID, uint64(i))
		if err := tx.Append(kv.RecentLocalTransaction, encID, txHashes[i].([]byte)); err != nil {
			return evicted, err
		}
	}

	if ASSERT {
		_ = tx.ForEach(kv.PooledTransaction, nil, func(k, v []byte) error {
			vv, err := tx.GetOne(kv.PooledSenderIDToAdress, v[:8])
			if err != nil {
				return err
			}
			if len(vv) == 0 {
				cc, _ := tx.Cursor(kv.PooledSenderIDToAdress)
				last, lastAddr, _ := cc.Last()
				if len(last) > 0 {
					fmt.Printf("last: %d,%x\n", binary.BigEndian.Uint64(last), lastAddr)
				}
				fmt.Printf("not foundd: %d,%x,%x,%x\n", binary.BigEndian.Uint64(v[:8]), k, v, vv)
				fmt.Printf("aa: %x,%x,%x\n", k, v, vv)
				panic("no-no")
			}
			return nil
		})
	}

	v := make([]byte, 0, 1024)
	for txHash, metaTx := range p.byHash {
		if metaTx.Tx.rlp == nil {
			continue
		}
		if ASSERT {
			if p.txNonce2Tx.count(metaTx.Tx.senderID) == 0 {
				panic("here i am")
			}
		}
		v = ensureEnoughSize(v, 8+8+len(metaTx.Tx.rlp))
		binary.BigEndian.PutUint64(v, metaTx.Tx.senderID)
		binary.BigEndian.PutUint64(v[8:], 0) // block num - timestamp
		copy(v[8+8:], metaTx.Tx.rlp)
		if ASSERT {
			if _, ok := p.senders.senderInfo[metaTx.Tx.senderID]; !ok {
				info, err := p.senders.info(metaTx.Tx.senderID, tx, false)
				if err != nil {
					panic(err)
				}
				if info == nil {
					panic("lost")
				}
			}
		}

		if err := tx.Put(kv.PooledTransaction, []byte(txHash), v); err != nil {
			return evicted, err
		}
		metaTx.Tx.rlp = nil
	}

	if ASSERT {
		txs := TxSlots{}
		parseCtx := NewTxParseContext()
		parseCtx.WithSender(false)
		i := 0
		hashID := [32]byte{}
		if err := tx.ForEach(kv.PooledTransaction, nil, func(k, v []byte) error {
			txs.Growth(i + 1)
			txs.txs[i] = &TxSlot{}

			_, err := parseCtx.ParseTransaction(v[8+8:], 0, txs.txs[i], nil)
			if err != nil {
				return fmt.Errorf("err: %w, rlp: %x\n", err, v[8+8:])
			}
			txs.txs[i].rlp = nil // means that we don't need store it in db anymore
			txs.txs[i].senderID = binary.BigEndian.Uint64(v)
			//bkock num = binary.BigEndian.Uint64(v[8:])
			copy(hashID[:], k)
			_, isLocalTx := p.localsHistory.Get(hashID)
			txs.isLocal[i] = isLocalTx

			if !p.txNonce2Tx.has(newMetaTx(txs.txs[i], txs.isLocal[i])) {
				panic("aaaaaa")
			}
			i++
			return nil
		}); err != nil {
			panic(err)
		}
	}

	binary.BigEndian.PutUint64(encID, p.protocolBaseFee.Load())
	if err := tx.Put(kv.PoolInfo, PoolProtocolBaseFeeKey, encID); err != nil {
		return evicted, err
	}
	binary.BigEndian.PutUint64(encID, p.pendingBaseFee.Load())
	if err := tx.Put(kv.PoolInfo, PoolPendingBaseFeeKey, encID); err != nil {
		return evicted, err
	}

	evicted, err = p.senders.flush(tx, p.txNonce2Tx, sendersWithoutTransactions, p.cfg.evictSendersAfterRounds)
	if err != nil {
		return evicted, err
	}

	// clean - in-memory data structure as later as possible - because if during this Tx will happen error,
	// DB will stay consitant but some in-memory structures may be alread cleaned, and retry will not work
	// failed write transaction must not create side-effects
	p.deletedTxs = p.deletedTxs[:0]

	return evicted, nil
}
func (p *TxPool) discardLocked(mt *metaTx) {
	delete(p.byHash, string(mt.Tx.idHash[:]))
	p.deletedTxs = append(p.deletedTxs, mt)
	p.txNonce2Tx.delete(mt)
	if mt.subPool&IsLocal != 0 {
		p.localsHistory.Add(mt.Tx.idHash, struct{}{})
	}

	if ASSERT && p.txNonce2Tx.tree.Len() != len(p.byHash) {
		panic(1)
	}
}

func (p *TxPool) fromDB(ctx context.Context, tx kv.RwTx, coreTx kv.Tx) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	if ASSERT {
		_ = tx.ForEach(kv.PooledTransaction, nil, func(k, v []byte) error {
			vv, err := tx.GetOne(kv.PooledSenderIDToAdress, v[:8])
			if err != nil {
				return err
			}
			if len(vv) == 0 {
				cc, _ := tx.Cursor(kv.PooledSenderIDToAdress)
				last, lastAddr, _ := cc.Last()
				fmt.Printf("last: %d,%x\n", binary.BigEndian.Uint64(last), lastAddr)
				fmt.Printf("now: %d\n", p.senders.senderID)
				fmt.Printf("not foundd: %d,%x,%x,%x\n", binary.BigEndian.Uint64(v[:8]), k, v, vv)
				panic("no-no")
			}
			return nil
		})
	}

	if err := p.senders.fromDB(ctx, tx, coreTx); err != nil {
		return err
	}

	if err := tx.ForEach(kv.RecentLocalTransaction, nil, func(k, v []byte) error {
		hashID := [32]byte{}
		copy(hashID[:], v)
		p.localsHistory.Add(hashID, struct{}{})
		return nil
	}); err != nil {
		return err
	}

	txs := TxSlots{}
	parseCtx := NewTxParseContext()
	parseCtx.WithSender(false)
	i := 0
	hashID := [32]byte{}
	if err := tx.ForEach(kv.PooledTransaction, nil, func(k, v []byte) error {
		txs.Growth(i + 1)
		txs.txs[i] = &TxSlot{}

		_, err := parseCtx.ParseTransaction(v[8+8:], 0, txs.txs[i], nil)
		if err != nil {
			return fmt.Errorf("err: %w, rlp: %x\n", err, v[8+8:])
		}
		txs.txs[i].rlp = nil // means that we don't need store it in db anymore
		txs.txs[i].senderID = binary.BigEndian.Uint64(v)

		senderAddr, err := tx.GetOne(kv.PooledSenderIDToAdress, v[:8])
		if err != nil {
			return err
		}
		if len(senderAddr) == 0 {
			panic("must not happen")
		}
		copy(txs.senders.At(i), senderAddr)
		//bkock num = binary.BigEndian.Uint64(v[8:])
		copy(hashID[:], k)
		_, isLocalTx := p.localsHistory.Get(hashID)
		txs.isLocal[i] = isLocalTx
		i++
		return nil
	}); err != nil {
		return err
	}

	var protocolBaseFee, pendingBaseFee uint64
	{
		v, err := tx.GetOne(kv.PoolInfo, PoolProtocolBaseFeeKey)
		if err != nil {
			return err
		}
		if len(v) > 0 {
			protocolBaseFee = binary.BigEndian.Uint64(v)
		}
	}
	{
		v, err := tx.GetOne(kv.PoolInfo, PoolPendingBaseFeeKey)
		if err != nil {
			return err
		}
		if len(v) > 0 {
			pendingBaseFee = binary.BigEndian.Uint64(v)
		}
	}
	cacheMisses, err := p.senders.onNewTxs(tx, txs)
	if err != nil {
		return err
	}
	if len(cacheMisses) > 0 {
		if err := p.senders.loadFromCore(coreTx, cacheMisses); err != nil {
			return err
		}
	}
	if err := onNewTxs(tx, p.senders, txs, protocolBaseFee, pendingBaseFee, p.pending, p.baseFee, p.queued, p.txNonce2Tx, p.byHash, p.discardLocked); err != nil {
		return err
	}
	p.pendingBaseFee.Store(pendingBaseFee)
	p.protocolBaseFee.Store(protocolBaseFee)

	return nil
}

func onNewBlock(tx kv.Tx, senders *SendersCache, unwindTxs TxSlots, minedTxs []*TxSlot, protocolBaseFee, pendingBaseFee uint64, pending, baseFee, queued *SubPool, byNonce *ByNonce, byHash map[string]*metaTx, discard func(*metaTx)) error {
	for i := range unwindTxs.txs {
		if unwindTxs.txs[i].senderID == 0 {
			return fmt.Errorf("onNewBlock.unwindTxs: senderID can't be zero")
		}
	}
	for i := range minedTxs {
		if minedTxs[i].senderID == 0 {
			return fmt.Errorf("onNewBlock.minedTxs: senderID can't be zero")
		}
	}

	if err := removeMined(byNonce, minedTxs, pending, baseFee, queued, discard); err != nil {
		return err
	}

	// This can be thought of a reverse operation from the one described before.
	// When a block that was deemed "the best" of its height, is no longer deemed "the best", the
	// transactions contained in it, are now viable for inclusion in other blocks, and therefore should
	// be returned into the transaction pool.
	// An interesting note here is that if the block contained any transactions local to the node,
	// by being first removed from the pool (from the "local" part of it), and then re-injected,
	// they effective lose their priority over the "remote" transactions. In order to prevent that,
	// somehow the fact that certain transactions were local, needs to be remembered for some
	// time (up to some "immutability threshold").
	changedSenders := unsafeAddToPendingPool(byNonce, unwindTxs, pending, baseFee, queued, byHash, discard)
	for id := range changedSenders {
		sender, err := senders.Info(id, tx, false)
		if err != nil {
			return err
		}
		onSenderChange(id, sender, byNonce, protocolBaseFee, pendingBaseFee)
	}

	pending.EnforceInvariants()
	baseFee.EnforceInvariants()
	queued.EnforceInvariants()

	promote(pending, baseFee, queued, discard)

	if ASSERT {
		byNonce.tree.Ascend(func(i btree.Item) bool {
			mt := i.(*sortByNonce).metaTx
			if mt.worstIndex < 0 {
				panic(1)
			}

			_, ok := byHash[string(mt.Tx.idHash[:])]
			if !ok {
				panic(3)
			}
			return true
		})
		for _, txn := range byHash {
			if !byNonce.has(txn) {
				panic(1)
			}
		}
	}
	if ASSERT && byNonce.tree.Len() != len(byHash) {
		panic(1)
	}

	return nil
}

// removeMined - apply new highest block (or batch of blocks)
//
// 1. New best block arrives, which potentially changes the balance and the nonce of some senders.
// We use senderIds data structure to find relevant senderId values, and then use senders data structure to
// modify state_balance and state_nonce, potentially remove some elements (if transaction with some nonce is
// included into a block), and finally, walk over the transaction records and update SubPool fields depending on
// the actual presence of nonce gaps and what the balance is.
func removeMined(byNonce *ByNonce, minedTxs []*TxSlot, pending, baseFee, queued *SubPool, discard func(tx *metaTx)) error {
	noncesToRemove := map[uint64]uint64{}
	for _, txn := range minedTxs {
		nonce, ok := noncesToRemove[txn.senderID]
		if !ok || txn.nonce > nonce {
			noncesToRemove[txn.senderID] = txn.nonce
		}
	}

	var toDel []*metaTx // can't delete items while iterate them
	for senderID, nonce := range noncesToRemove {
		//if sender.txNonce2Tx.Len() > 0 {
		//log.Debug("[txpool] removing mined", "senderID", tx.senderID, "sender.txNonce2Tx.len()", sender.txNonce2Tx.Len())
		//}
		// delete mined transactions from everywhere
		byNonce.ascend(senderID, func(mt *metaTx) bool {
			//log.Debug("[txpool] removing mined, cmp nonces", "tx.nonce", it.metaTx.Tx.nonce, "sender.nonce", sender.nonce)
			if mt.Tx.nonce > nonce {
				return false
			}
			toDel = append(toDel, mt)
			// del from sub-pool
			switch mt.currentSubPool {
			case PendingSubPool:
				pending.UnsafeRemove(mt)
			case BaseFeeSubPool:
				baseFee.UnsafeRemove(mt)
			case QueuedSubPool:
				queued.UnsafeRemove(mt)
			default:
				//already removed
			}
			return true
		})

		for i := range toDel {
			discard(toDel[i])
		}
		toDel = toDel[:0]
	}
	return nil
}

// unwind
func unsafeAddToPendingPool(byNonce *ByNonce, newTxs TxSlots, pending, baseFee, queued *SubPool, byHash map[string]*metaTx, discard func(tx *metaTx)) (changedSenders map[uint64]struct{}) {
	changedSenders = map[uint64]struct{}{}
	for i, txn := range newTxs.txs {
		if _, ok := byHash[string(txn.idHash[:])]; ok {
			continue
		}
		mt := newMetaTx(txn, newTxs.isLocal[i])

		// Insert to pending pool, if pool doesn't have txn with same Nonce and bigger Tip
		found := byNonce.get(txn.senderID, txn.nonce)
		if found != nil {
			if txn.tip <= found.Tx.tip {
				continue
			}

			switch found.currentSubPool {
			case PendingSubPool:
				pending.UnsafeRemove(found)
			case BaseFeeSubPool:
				baseFee.UnsafeRemove(found)
			case QueuedSubPool:
				queued.UnsafeRemove(found)
			default:
				//already removed
			}

			discard(found)
		}

		byHash[string(txn.idHash[:])] = mt
		if replaced := byNonce.replaceOrInsert(mt); replaced != nil {
			if ASSERT {
				panic("must neve happen")
			}
		}

		changedSenders[txn.senderID] = struct{}{}
		pending.UnsafeAdd(mt)
	}
	if ASSERT {
		for _, txn := range byHash {
			if !byNonce.has(txn) {
				panic(1)
			}
		}
	}
	if ASSERT && byNonce.tree.Len() != len(byHash) {
		panic(1)
	}
	return changedSenders
}

func onSenderChange(senderID uint64, sender *senderInfo, byNonce *ByNonce, protocolBaseFee, pendingBaseFee uint64) {
	if ASSERT && sender == nil {
		fmt.Printf("nil sender info: %d\n", senderID)
	}
	noGapsNonce := sender.nonce + 1
	cumulativeRequiredBalance := uint256.NewInt(0)
	minFeeCap := uint64(math.MaxUint64)
	minTip := uint64(math.MaxUint64)
	byNonce.ascend(senderID, func(mt *metaTx) bool {
		// Sender has enough balance for: gasLimit x feeCap + transferred_value
		needBalance := uint256.NewInt(mt.Tx.gas)
		needBalance.Mul(needBalance, uint256.NewInt(mt.Tx.feeCap))
		needBalance.Add(needBalance, &mt.Tx.value)
		minFeeCap = min(minFeeCap, mt.Tx.feeCap)
		minTip = min(minTip, mt.Tx.tip)
		if pendingBaseFee >= minFeeCap {
			mt.effectiveTip = minTip
		} else {
			mt.effectiveTip = minFeeCap - pendingBaseFee
		}
		// 1. Minimum fee requirement. Set to 1 if feeCap of the transaction is no less than in-protocol
		// parameter of minimal base fee. Set to 0 if feeCap is less than minimum base fee, which means
		// this transaction will never be included into this particular chain.
		mt.subPool &^= EnoughFeeCapProtocol
		if mt.Tx.feeCap >= protocolBaseFee {
			//fmt.Printf("alex1: %d,%d,%d,%d\n", mt.NeedBalance.Uint64(), mt.Tx.gas, mt.Tx.feeCap, mt.Tx.value.Uint64())
			//fmt.Printf("alex2: %d,%t\n", sender.balance.Uint64(), mt.SenderHasEnoughBalance)
			mt.subPool |= EnoughFeeCapProtocol
		} else {
			mt.subPool = 0 // TODO: we immediately drop all transactions if they have no first bit - then maybe we don't need this bit at all? And don't add such transactions to queue?
			return true
		}

		// 2. Absence of nonce gaps. Set to 1 for transactions whose nonce is N, state nonce for
		// the sender is M, and there are transactions for all nonces between M and N from the same
		// sender. Set to 0 is the transaction's nonce is divided from the state nonce by one or more nonce gaps.
		mt.subPool &^= NoNonceGaps
		if noGapsNonce == mt.Tx.nonce {
			mt.subPool |= NoNonceGaps
			noGapsNonce++
		}

		// 3. Sufficient balance for gas. Set to 1 if the balance of sender's account in the
		// state is B, nonce of the sender in the state is M, nonce of the transaction is N, and the
		// sum of feeCap x gasLimit + transferred_value of all transactions from this sender with
		// nonces N+1 ... M is no more than B. Set to 0 otherwise. In other words, this bit is
		// set if there is currently a guarantee that the transaction and all its required prior
		// transactions will be able to pay for gas.
		mt.subPool &^= EnoughBalance
		if mt.Tx.nonce > sender.nonce {
			cumulativeRequiredBalance = cumulativeRequiredBalance.Add(cumulativeRequiredBalance, needBalance) // already deleted all transactions with nonce <= sender.nonce
			if sender.balance.Gt(cumulativeRequiredBalance) || sender.balance.Eq(cumulativeRequiredBalance) {
				mt.subPool |= EnoughBalance
			}
		}

		// 4. Dynamic fee requirement. Set to 1 if feeCap of the transaction is no less than
		// baseFee of the currently pending block. Set to 0 otherwise.
		mt.subPool &^= EnoughFeeCapBlock
		if mt.Tx.feeCap >= pendingBaseFee {
			mt.subPool |= EnoughFeeCapBlock
		}

		// 5. Local transaction. Set to 1 if transaction is local.
		// can't change

		return true
	})
}

func promote(pending, baseFee, queued *SubPool, discard func(tx *metaTx)) {
	//1. If top element in the worst green queue has subPool != 0b1111 (binary), it needs to be removed from the green pool.
	//   If subPool < 0b1000 (not satisfying minimum fee), discard.
	//   If subPool == 0b1110, demote to the yellow pool, otherwise demote to the red pool.
	for worst := pending.Worst(); pending.Len() > 0; worst = pending.Worst() {
		if worst.subPool >= 0b11110 {
			break
		}
		if worst.subPool >= 0b11100 {
			baseFee.Add(pending.PopWorst())
			continue
		}
		if worst.subPool >= 0b10000 {
			queued.Add(pending.PopWorst())
			continue
		}
		discard(pending.PopWorst())
	}

	//2. If top element in the worst green queue has subPool == 0b1111, but there is not enough room in the pool, discard.
	for worst := pending.Worst(); pending.Len() > PendingSubPoolLimit; worst = pending.Worst() {
		if worst.subPool >= 0b11111 { // TODO: here must 'subPool == 0b1111' or 'subPool <= 0b1111' ?
			break
		}
		discard(pending.PopWorst())
	}

	//3. If the top element in the best yellow queue has subPool == 0b1111, promote to the green pool.
	for best := baseFee.Best(); baseFee.Len() > 0; best = baseFee.Best() {
		if best.subPool < 0b11110 {
			break
		}
		pending.Add(baseFee.PopBest())
	}

	//4. If the top element in the worst yellow queue has subPool != 0x1110, it needs to be removed from the yellow pool.
	//   If subPool < 0b1000 (not satisfying minimum fee), discard. Otherwise, demote to the red pool.
	for worst := baseFee.Worst(); baseFee.Len() > 0; worst = baseFee.Worst() {
		if worst.subPool >= 0b11100 {
			break
		}
		if worst.subPool >= 0b10000 {
			queued.Add(baseFee.PopWorst())
			continue
		}
		discard(baseFee.PopWorst())
	}

	//5. If the top element in the worst yellow queue has subPool == 0x1110, but there is not enough room in the pool, discard.
	for worst := baseFee.Worst(); baseFee.Len() > BaseFeeSubPoolLimit; worst = baseFee.Worst() {
		if worst.subPool >= 0b11110 {
			break
		}
		discard(baseFee.PopWorst())
	}

	//6. If the top element in the best red queue has subPool == 0x1110, promote to the yellow pool. If subPool == 0x1111, promote to the green pool.
	for best := queued.Best(); queued.Len() > 0; best = queued.Best() {
		if best.subPool < 0b11100 {
			break
		}
		if best.subPool < 0b11110 {
			baseFee.Add(queued.PopBest())
			continue
		}

		pending.Add(queued.PopBest())
	}

	//7. If the top element in the worst red queue has subPool < 0b1000 (not satisfying minimum fee), discard.
	for worst := queued.Worst(); queued.Len() > 0; worst = queued.Worst() {
		if worst.subPool >= 0b10000 {
			break
		}

		discard(queued.PopWorst())
	}

	//8. If the top element in the worst red queue has subPool >= 0b100, but there is not enough room in the pool, discard.
	for _ = queued.Worst(); queued.Len() > QueuedSubPoolLimit; _ = queued.Worst() {
		discard(queued.PopWorst())
	}
}

type SubPool struct {
	t     SubPoolType
	best  *BestQueue
	worst *WorstQueue
}

func NewSubPool(t SubPoolType) *SubPool {
	return &SubPool{t: t, best: &BestQueue{}, worst: &WorstQueue{}}
}

func (p *SubPool) EnforceInvariants() {
	heap.Init(p.worst)
	heap.Init(p.best)
}
func (p *SubPool) Best() *metaTx {
	if len(*p.best) == 0 {
		return nil
	}
	return (*p.best)[0]
}
func (p *SubPool) Worst() *metaTx {
	if len(*p.worst) == 0 {
		return nil
	}
	return (*p.worst)[0]
}
func (p *SubPool) PopBest() *metaTx {
	i := heap.Pop(p.best).(*metaTx)
	heap.Remove(p.worst, i.worstIndex)
	return i
}
func (p *SubPool) PopWorst() *metaTx {
	i := heap.Pop(p.worst).(*metaTx)
	heap.Remove(p.best, i.bestIndex)
	return i
}
func (p *SubPool) Len() int { return p.best.Len() }
func (p *SubPool) Add(i *metaTx) {
	i.currentSubPool = p.t
	heap.Push(p.best, i)
	heap.Push(p.worst, i)
}

// UnsafeRemove - does break Heap invariants, but it has O(1) instead of O(log(n)) complexity.
// Must manually call heap.Init after such changes.
// Make sense to batch unsafe changes
func (p *SubPool) UnsafeRemove(i *metaTx) {
	if p.Len() == 0 {
		return
	}
	if p.Len() == 1 && i.bestIndex == 0 {
		p.worst.Pop()
		p.best.Pop()
		return
	}
	// manually call funcs instead of heap.Pop
	p.worst.Swap(i.worstIndex, p.worst.Len()-1)
	p.worst.Pop()
	p.best.Swap(i.bestIndex, p.best.Len()-1)
	p.best.Pop()
}
func (p *SubPool) UnsafeAdd(i *metaTx) {
	i.currentSubPool = p.t
	p.worst.Push(i)
	p.best.Push(i)
}
func (p *SubPool) DebugPrint(prefix string) {
	for i, it := range *p.best {
		fmt.Printf("%s.best: %d, %d, %d\n", prefix, i, it.subPool, it.bestIndex)
	}
	for i, it := range *p.worst {
		fmt.Printf("%s.worst: %d, %d, %d\n", prefix, i, it.subPool, it.worstIndex)
	}
}

type BestQueue []*metaTx

func (mt *metaTx) Less(than *metaTx) bool {
	if mt.subPool != than.subPool {
		return mt.subPool < than.subPool
	}

	if mt.effectiveTip != than.effectiveTip {
		return mt.effectiveTip < than.effectiveTip
	}

	if mt.Tx.nonce != than.Tx.nonce {
		return mt.Tx.nonce < than.Tx.nonce
	}
	return false
}

func (p BestQueue) Len() int           { return len(p) }
func (p BestQueue) Less(i, j int) bool { return !p[i].Less(p[j]) } // We want Pop to give us the highest, not lowest, priority so we use !less here.
func (p BestQueue) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
	p[i].bestIndex = i
	p[j].bestIndex = j
}
func (p *BestQueue) Push(x interface{}) {
	n := len(*p)
	item := x.(*metaTx)
	item.bestIndex = n
	*p = append(*p, item)
}

func (p *BestQueue) Pop() interface{} {
	old := *p
	n := len(old)
	item := old[n-1]
	old[n-1] = nil          // avoid memory leak
	item.bestIndex = -1     // for safety
	item.currentSubPool = 0 // for safety
	*p = old[0 : n-1]
	return item
}

type WorstQueue []*metaTx

func (p WorstQueue) Len() int           { return len(p) }
func (p WorstQueue) Less(i, j int) bool { return p[i].Less(p[j]) }
func (p WorstQueue) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
	p[i].worstIndex = i
	p[j].worstIndex = j
}
func (p *WorstQueue) Push(x interface{}) {
	n := len(*p)
	item := x.(*metaTx)
	item.worstIndex = n
	*p = append(*p, x.(*metaTx))
}
func (p *WorstQueue) Pop() interface{} {
	old := *p
	n := len(old)
	item := old[n-1]
	old[n-1] = nil          // avoid memory leak
	item.worstIndex = -1    // for safety
	item.currentSubPool = 0 // for safety
	*p = old[0 : n-1]
	return item
}

// BroadcastLoop - does:
// send pending byHash to p2p:
//      - new byHash
//      - all pooled byHash to recently connected peers
//      - all local pooled byHash to random peers periodically
// promote/demote transactions
// reorgs
func BroadcastLoop(ctx context.Context, db kv.RwDB, coreDB kv.RoDB, p *TxPool, senders *SendersCache, newTxs chan Hashes, send *Send) {
	//db.Update(ctx, func(tx kv.RwTx) error { return tx.ClearBucket(kv.PooledSender) })
	if err := db.Update(ctx, func(tx kv.RwTx) error {
		return coreDB.View(ctx, func(coreTx kv.Tx) error {
			return p.fromDB(ctx, tx, coreTx)
		})
	}); err != nil {
		log.Error("restore from db", "err", err)
	}
	if err := db.View(ctx, func(tx kv.Tx) error { return p.logStats(tx) }); err != nil {
		log.Error("log stats", "err", err)
	}
	if ASSERT {
		go func() {
			if err := p.forceCheckState(ctx, db, coreDB); err != nil {
				log.Error("forceCheckState", "err", err)
			}
		}()
	}

	logEvery := time.NewTicker(p.cfg.logEvery)
	defer logEvery.Stop()
	commitEvery := time.NewTicker(p.cfg.commitEvery)
	defer commitEvery.Stop()

	syncToNewPeersEvery := time.NewTicker(p.cfg.syncToNewPeersEvery)
	defer syncToNewPeersEvery.Stop()

	localTxHashes := make([]byte, 0, 128)
	remoteTxHashes := make([]byte, 0, 128)

	for {
		select {
		case <-ctx.Done():
			return
		case <-logEvery.C:
			if err := db.View(ctx, func(tx kv.Tx) error { return p.logStats(tx) }); err != nil {
				log.Error("log stats", "err", err)
			}
		case <-commitEvery.C:
			if db != nil {
				t := time.Now()
				evicted, written, err := p.flush(db)
				if err != nil {
					log.Error("flush is local history", "err", err)
					continue
				}
				log.Info("flush", "written_kb", written/1024, "evicted", evicted, "in", time.Since(t))
			}
		case h := <-newTxs:
			// first broadcast all local txs to all peers, then non-local to random sqrt(peersAmount) peers
			localTxHashes = localTxHashes[:0]
			remoteTxHashes = remoteTxHashes[:0]

			for i := 0; i < h.Len(); i++ {
				if p.IdHashIsLocal(h.At(i)) {
					localTxHashes = append(localTxHashes, h.At(i)...)
				} else {
					remoteTxHashes = append(localTxHashes, h.At(i)...)
				}
			}

			send.BroadcastLocalPooledTxs(localTxHashes)
			send.BroadcastRemotePooledTxs(remoteTxHashes)
		case <-syncToNewPeersEvery.C: // new peer
			newPeers := p.recentlyConnectedPeers.GetAndClean()
			if len(newPeers) == 0 {
				continue
			}
			remoteTxHashes = p.AppendAllHashes(remoteTxHashes[:0])
			send.PropagatePooledTxsToPeersList(newPeers, remoteTxHashes)
		}
	}
}

func coreProgress(coreTx kv.Tx) (uint64, error) {
	stageProgress, err := coreTx.GetOne(kv.SyncStageProgress, []byte("Finish"))
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(stageProgress), err
}

func (p *TxPool) forceCheckState(ctx context.Context, db, coreDB kv.RoDB) error {
	for {
		if err := db.View(ctx, func(tx kv.Tx) error {
			return coreDB.View(ctx, func(coreTx kv.Tx) error {
				return tx.ForPrefix(kv.PooledSender, nil, func(k, v []byte) error {
					remoteProgress, err := coreProgress(coreTx)
					if err != nil {
						return err
					}
					if remoteProgress != p.senders.blockHeight.Load() { // skip
						return nil
					}
					v2, err := coreTx.GetOne(kv.PlainState, k)
					if err != nil {
						return err
					}
					if v2 == nil {
						// for now skip this case because we do create
						// account with 0 nonce and 0 balance for unknown senders (because
						// they may become known in near future)
						// But we need maybe store them as a thumbstone - to separate
						// deleted accounts from not know
						return nil
					}
					if !bytes.Equal(v, v2) {
						return fmt.Errorf("state check failed: key=%x, local value=%x, remote value=%x\n", k, v, v2)
					}
					return nil
				})
			})
		}); err != nil {
			return err
		}
	}
}

//nolint
func copyBytes(b []byte) (copiedBytes []byte) {
	copiedBytes = make([]byte, len(b))
	copy(copiedBytes, b)
	return
}

// recentlyConnectedPeers does buffer IDs of recently connected good peers
// then sync of pooled Transaction can happen to all of then at once
// DoS protection and performance saving
// it doesn't track if peer disconnected, it's fine
type recentlyConnectedPeers struct {
	lock  sync.RWMutex
	peers []PeerID
}

func (l *recentlyConnectedPeers) AddPeer(p PeerID) {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.peers = append(l.peers, p)
}

func (l *recentlyConnectedPeers) GetAndClean() []PeerID {
	l.lock.Lock()
	defer l.lock.Unlock()
	peers := l.peers
	l.peers = nil
	return peers
}

func min(a, b uint64) uint64 {
	if a <= b {
		return a
	}
	return b
}