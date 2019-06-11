// Copyright (C) 2019 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package data

import (
	"context"
	"fmt"
	"sync"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/pools"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/network"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/execpool"
	"github.com/algorand/go-algorand/util/metrics"
)

// The size txBacklogSize used to determine the size of the backlog that is used to store incoming transaction messages before starting dropping them.
// It should be configured to be higher then the number of CPU cores, so that the execution pool get saturated, but not too high to avoid lockout of the
// execution pool for a long duration of time.
const txBacklogSize = 1000

var transactionMessagesHandled = metrics.MakeCounter(metrics.TransactionMessagesHandled)
var transactionMessagesDroppedFromBacklog = metrics.MakeCounter(metrics.TransactionMessagesDroppedFromBacklog)
var transactionMessagesDroppedFromPool = metrics.MakeCounter(metrics.TransactionMessagesDroppedFromPool)

// The txBacklogMsg structure used to track a single incoming transaction from the gossip network,
type txBacklogMsg struct {
	rawmsg          *network.IncomingMessage      // the raw message from the network
	unverifiedTxn   *transactions.SignedTxn       // the unverified ( and signed ) transaction
	proto           config.ConsensusParams        // the consensus parameters that corresponds to the latest round. Filled in during checkAlreadyCommitted execution.
	spec            transactions.SpecialAddresses // corresponds to the latest round
	verificationErr error                         // The verification error generated by the verification function, if any.
}

// TxHandler handles transaction messages
type TxHandler struct {
	txPool                *pools.TransactionPool
	ledger                *Ledger
	genesisID             string
	genesisHash           crypto.Digest
	txVerificationPool    execpool.BacklogPool
	backlogQueue          chan *txBacklogMsg
	postVerificationQueue chan *txBacklogMsg
	backlogWg             sync.WaitGroup
	net                   network.GossipNode
	ctx                   context.Context
	ctxCancel             context.CancelFunc
}

// MakeTxHandler makes a new handler for transaction messages
func MakeTxHandler(txPool *pools.TransactionPool, ledger *Ledger, net network.GossipNode, genesisID string, genesisHash crypto.Digest, executionPool execpool.BacklogPool) *TxHandler {

	if txPool == nil {
		logging.Base().Fatal("MakeTxHandler: txPool is nil on initialization")
		return nil
	}

	if ledger == nil {
		logging.Base().Fatal("MakeTxHandler: ledger is nil on initialization")
		return nil
	}

	handler := &TxHandler{
		txPool:                txPool,
		genesisID:             genesisID,
		genesisHash:           genesisHash,
		ledger:                ledger,
		txVerificationPool:    executionPool,
		backlogQueue:          make(chan *txBacklogMsg, txBacklogSize),
		postVerificationQueue: make(chan *txBacklogMsg, txBacklogSize),
		net:                   net,
	}

	net.RegisterHandlers([]network.TaggedMessageHandler{
		network.TaggedMessageHandler{Tag: protocol.TxnTag, MessageHandler: network.HandlerFunc(handler.processIncomingTxn)},
	})

	handler.ctx, handler.ctxCancel = context.WithCancel(context.Background())
	return handler
}

// Start enables the processing of incoming messages at the transaction handler
func (handler *TxHandler) Start() {
	handler.backlogWg.Add(1)
	go handler.backlogWorker()
}

// Stop suspends the processing of incoming messages at the transaction handler
func (handler *TxHandler) Stop() {
	handler.ctxCancel()
	handler.backlogWg.Wait()
}

// backlogWorker is the worker go routine that process the incoming messages from the postVerificationQueue and backlogQueue channels
// and dispatches them further.
func (handler *TxHandler) backlogWorker() {
	defer handler.backlogWg.Done()
	for {
		// prioritize the postVerificationQueue
		select {
		case wi, ok := <-handler.postVerificationQueue:
			if !ok {
				return
			}
			if wi.verificationErr != nil {
				// disconnect from peer.
				logging.Base().Warnf("Received a malformed txn %v: %v", wi.unverifiedTxn, wi.verificationErr)
				handler.net.Disconnect(wi.rawmsg.Sender)
				continue
			}
			// at this point, we've verified the transaction, so we can safely treat the transaction as a verified transaction.
			verifiedTxn := wi.unverifiedTxn

			// save the transaction, if it has high enough fee and not already in the cache
			err := handler.txPool.Remember(*verifiedTxn)
			if err != nil {
				logging.Base().Debugf("could not remember tx: %v", err)
				continue
			}
			handler.net.Relay(handler.ctx, protocol.TxnTag, wi.rawmsg.Data, false, wi.rawmsg.Sender)

			// restart the loop so that we could empty out the post verification queue.
			continue
		default:
		}

		// we have no more post verification items. wait for either backlog queue item or post verification item.
		select {
		case wi, ok := <-handler.backlogQueue:
			if !ok {
				return
			}
			if handler.checkAlreadyCommitted(wi) {
				continue
			}
			// enqueue the task to the verification pool.
			handler.txVerificationPool.EnqueueBacklog(handler.ctx, handler.asyncVerifySignature, wi, nil)
		case wi, ok := <-handler.postVerificationQueue:
			if !ok {
				return
			}
			if wi.verificationErr != nil {
				// disconnect from peer.
				logging.Base().Warnf("Received a malformed txn %v: %v", wi.unverifiedTxn, wi.verificationErr)
				handler.net.Disconnect(wi.rawmsg.Sender)
				continue
			}

			// we've processed this message, so increase the counter.
			transactionMessagesHandled.Inc(nil)

			// at this point, we've verified the transaction, so we can safely treat the transaction as a verified transaction.
			verifiedTxn := wi.unverifiedTxn

			// save the transaction, if it has high enough fee and not already in the cache
			err := handler.txPool.Remember(*verifiedTxn)
			if err != nil {
				logging.Base().Debugf("could not remember tx: %v", err)
				continue
			}
			handler.net.Relay(handler.ctx, protocol.TxnTag, wi.rawmsg.Data, false, wi.rawmsg.Sender)
		case <-handler.ctx.Done():
			return
		}
	}
}

// asyncVerifySignature verifies that the given transaction is valid, and update the txBacklogMsg data structure accordingly.
func (handler *TxHandler) asyncVerifySignature(arg interface{}) interface{} {
	tx := arg.(*txBacklogMsg)
	tx.verificationErr = tx.unverifiedTxn.Verify(tx.spec, tx.proto)
	select {
	case handler.postVerificationQueue <- tx:
	default:
		// we failed to write to the output queue, since the queue was full.
		// adding the metric here allows us to monitor how frequently it happens.
		transactionMessagesDroppedFromPool.Inc(nil)
	}
	return nil
}

func (handler *TxHandler) processIncomingTxn(rawmsg network.IncomingMessage) network.OutgoingMessage {
	var unverifiedTxn transactions.SignedTxn
	err := protocol.Decode(rawmsg.Data, &unverifiedTxn)
	if err != nil {
		logging.Base().Warnf("Received a non-decodable txn: %v", err)
		return network.OutgoingMessage{Action: network.Disconnect}
	}

	select {
	case handler.backlogQueue <- &txBacklogMsg{
		rawmsg:        &rawmsg,
		unverifiedTxn: &unverifiedTxn,
	}:
	default:
		// if we failed here we want to increase the corresponding metric. It might suggest that we
		// want to increase the queue size.
		transactionMessagesDroppedFromBacklog.Inc(nil)
	}

	return network.OutgoingMessage{Action: network.Ignore}
}

// checkAlreadyCommitted test to see if the given transaction ( in the txBacklogMsg ) was already commited, and
// whether it would qualify as a candidate for the transaction pool.
func (handler *TxHandler) checkAlreadyCommitted(tx *txBacklogMsg) (processingDone bool) {
	tx.unverifiedTxn.Txn.InitCaches()
	logging.Base().Debugf("got a tx with ID %v", tx.unverifiedTxn.ID())

	// do a quick test to check that this transaction could potentially be committed, to reject dup pending transactions
	err := handler.txPool.Test(*tx.unverifiedTxn)
	if err != nil {
		logging.Base().Debugf("txPool rejected transaction: %v", err)
		return true
	}

	// build the transaction verification context
	latest := handler.ledger.Latest()
	latestHdr, err := handler.ledger.BlockHdr(latest)
	if err != nil {
		logging.Base().Warnf("Could not get header for previous block %v: %v", latest, err)
		return true
	}
	tx.proto = config.Consensus[latestHdr.CurrentProtocol]
	tx.spec.FeeSink = latestHdr.FeeSink
	tx.spec.RewardsPool = latestHdr.RewardsPool

	tc := transactions.ExplicitTxnContext{
		ExplicitRound: latest + 1,
		Proto:         tx.proto,
		GenID:         handler.genesisID,
		GenHash:       handler.genesisHash,
	}

	err = tx.unverifiedTxn.Txn.Alive(tc)
	if err != nil {
		logging.Base().Debugf("Received a dead txn %s: %v", tx.unverifiedTxn.ID(), err)
		return true
	}

	committed, err := handler.ledger.Committed(*tx.unverifiedTxn)
	if err != nil {
		logging.Base().Errorf("Could not verify committed status of txn %v: %v", tx.unverifiedTxn, err)
		return true
	}

	if committed {
		logging.Base().Debugf("Already confirmed tx %v", tx.unverifiedTxn.ID())
		return true
	}
	return false
}

func (handler *TxHandler) processDecoded(unverifiedTxn transactions.SignedTxn) (outmsg network.OutgoingMessage, processingDone bool) {
	tx := &txBacklogMsg{
		unverifiedTxn: &unverifiedTxn,
	}
	if handler.checkAlreadyCommitted(tx) {
		return network.OutgoingMessage{}, true
	}

	err := tx.unverifiedTxn.PoolVerify(tx.spec, tx.proto, handler.txVerificationPool)
	if err != nil {
		// transaction is invalid
		logging.Base().Warnf("Received a malformed txn %v: %v", unverifiedTxn, err)
		return network.OutgoingMessage{Action: network.Disconnect}, true
	}

	// at this point, we've verified the transaction, so we can safely treat the transaction as a verified transaction.
	verifiedTxn := unverifiedTxn

	// save the transaction, if it has high enough fee and not already in the cache
	err = handler.txPool.Remember(verifiedTxn)
	if err != nil {
		logging.Base().Debugf("could not remember tx: %v", err)
		return network.OutgoingMessage{}, true
	}
	return network.OutgoingMessage{}, false
}

// SolicitedTxHandler handles messages received through channels other than the gossip network.
// It therefore circumvents the notion of incoming/outgoing messages
type SolicitedTxHandler interface {
	Handle(txn transactions.SignedTxn) error
}

// SolicitedTxHandler converts a transaction handler to a SolicitedTxHandler
func (handler *TxHandler) SolicitedTxHandler() SolicitedTxHandler {
	return &solicitedTxHandler{txHandler: handler}
}

type solicitedTxHandler struct {
	txHandler *TxHandler
}

func (handler *solicitedTxHandler) Handle(txn transactions.SignedTxn) error {
	outmsg, _ := handler.txHandler.processDecoded(txn)
	if outmsg.Action == network.Disconnect {
		return fmt.Errorf("invlid transaction")
	}
	return nil
}