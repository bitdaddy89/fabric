/*
Copyright IBM Corp. 2016 All Rights Reserved.

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

package kvledger

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/ledger/blkstorage"
	"github.com/hyperledger/fabric/core/ledger/blkstorage/fsblkstorage"
	"github.com/hyperledger/fabric/core/ledger/history"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/couchdbtxmgmt"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/lockbasedtxmgmt"
	"github.com/hyperledger/fabric/core/ledger/ledgerconfig"

	logging "github.com/op/go-logging"

	"github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric/protos/peer"
)

var logger = logging.MustGetLogger("kvledger")

// Conf captures `KVLedger` configurations
type Conf struct {
	blockStorageDir  string
	maxBlockfileSize int
	txMgrDBPath      string
}

// NewConf constructs new `Conf`.
// filesystemPath is the top level directory under which `KVLedger` manages its data
func NewConf(filesystemPath string, maxBlockfileSize int) *Conf {
	if !strings.HasSuffix(filesystemPath, "/") {
		filesystemPath = filesystemPath + "/"
	}
	blocksStorageDir := filesystemPath + "blocks"
	txMgrDBPath := filesystemPath + "txMgmgt/db"
	return &Conf{blocksStorageDir, maxBlockfileSize, txMgrDBPath}
}

// KVLedger provides an implementation of `ledger.ValidatedLedger`.
// This implementation provides a key-value based data model
type KVLedger struct {
	blockStore           blkstorage.BlockStore
	txtmgmt              txmgmt.TxMgr
	historymgmt          history.HistMgr
	pendingBlockToCommit *common.Block
}

// NewKVLedger constructs new `KVLedger`
func NewKVLedger(conf *Conf) (*KVLedger, error) {

	logger.Debugf("Creating KVLedger using config: ", conf)

	attrsToIndex := []blkstorage.IndexableAttr{
		blkstorage.IndexableAttrBlockHash,
		blkstorage.IndexableAttrBlockNum,
		blkstorage.IndexableAttrTxID,
		blkstorage.IndexableAttrBlockNumTranNum,
	}
	indexConfig := &blkstorage.IndexConfig{AttrsToIndex: attrsToIndex}
	blockStorageConf := fsblkstorage.NewConf(conf.blockStorageDir, conf.maxBlockfileSize)
	blockStore := fsblkstorage.NewFsBlockStore(blockStorageConf, indexConfig)

	//State and History database managers
	var txmgmt txmgmt.TxMgr
	var historymgmt history.HistMgr

	if ledgerconfig.IsCouchDBEnabled() == true {
		//By default we can talk to CouchDB with empty id and pw (""), or you can add your own id and password to talk to a secured CouchDB
		logger.Debugf("===COUCHDB=== NewKVLedger() Using CouchDB instead of RocksDB...hardcoding and passing connection config for now")

		couchDBDef := ledgerconfig.GetCouchDBDefinition()

		//create new transaction manager based on couchDB
		txmgmt = couchdbtxmgmt.NewCouchDBTxMgr(&couchdbtxmgmt.Conf{DBPath: conf.txMgrDBPath},
			couchDBDef.URL,      //couchDB connection URL
			"system",            //couchDB db name matches ledger name, TODO for now use system ledger, eventually allow passing in subledger name
			couchDBDef.Username, //enter couchDB id here
			couchDBDef.Password) //enter couchDB pw here
	} else {
		// Fall back to using RocksDB lockbased transaction manager
		txmgmt = lockbasedtxmgmt.NewLockBasedTxMgr(&lockbasedtxmgmt.Conf{DBPath: conf.txMgrDBPath})
	}

	if ledgerconfig.IsHistoryDBEnabled() == true {
		logger.Debugf("===HISTORYDB=== NewKVLedger() Using CouchDB for transaction history database")

		couchDBDef := ledgerconfig.GetCouchDBDefinition()

		historymgmt = history.NewCouchDBHistMgr(
			couchDBDef.URL,      //couchDB connection URL
			"system_history",    //couchDB db name matches ledger name, TODO for now use system_history ledger, eventually allow passing in subledger name
			couchDBDef.Username, //enter couchDB id here
			couchDBDef.Password) //enter couchDB pw here
	}

	l := &KVLedger{blockStore, txmgmt, historymgmt, nil}

	if err := recoverStateDB(l); err != nil {
		panic(fmt.Errorf(`Error during state DB recovery:%s`, err))
	}

	return l, nil
}

//Recover the state database by recommitting last valid blocks
func recoverStateDB(l *KVLedger) error {
	//If there is no block in blockstorage, nothing to recover.
	info, _ := l.blockStore.GetBlockchainInfo()
	if info.Height == 0 {
		return nil
	}

	//Getting savepointValue stored in the state DB
	var err error
	var savepointValue uint64
	if savepointValue, err = l.txtmgmt.GetBlockNumFromSavepoint(); err != nil {
		return err
	}

	//Checking whether the savepointValue is in sync with block storage height
	if savepointValue == info.Height {
		return nil
	} else if savepointValue > info.Height {
		return errors.New("BlockStorage height is behind savepoint by %d blocks. Recovery the BlockStore first")
	}

	//Compute updateSet for each missing savepoint and commit to state DB
	for blockNumber := savepointValue + 1; blockNumber <= info.Height; blockNumber++ {
		if l.pendingBlockToCommit, err = l.GetBlockByNumber(blockNumber); err != nil {
			return err
		}
		logger.Debugf("Constructing updateSet for the block %d", blockNumber)
		if _, _, err = l.txtmgmt.ValidateAndPrepare(l.pendingBlockToCommit, false); err != nil {
			return err
		}
		logger.Debugf("Committing block %d to state database", blockNumber)
		if err = l.txtmgmt.Commit(); err != nil {
			return err
		}
	}
	l.pendingBlockToCommit = nil

	return nil
}

// GetTransactionByID retrieves a transaction by id
func (l *KVLedger) GetTransactionByID(txID string) (*pb.Transaction, error) {
	return l.blockStore.RetrieveTxByID(txID)
}

// GetBlockchainInfo returns basic info about blockchain
func (l *KVLedger) GetBlockchainInfo() (*pb.BlockchainInfo, error) {
	return l.blockStore.GetBlockchainInfo()
}

// GetBlockByNumber returns block at a given height
// blockNumber of  math.MaxUint64 will return last block
func (l *KVLedger) GetBlockByNumber(blockNumber uint64) (*common.Block, error) {
	return l.blockStore.RetrieveBlockByNumber(blockNumber)

}

// GetBlocksIterator returns an iterator that starts from `startBlockNumber`(inclusive).
// The iterator is a blocking iterator i.e., it blocks till the next block gets available in the ledger
// ResultsIterator contains type BlockHolder
func (l *KVLedger) GetBlocksIterator(startBlockNumber uint64) (ledger.ResultsIterator, error) {
	return l.blockStore.RetrieveBlocks(startBlockNumber)

}

// GetBlockByHash returns a block given it's hash
func (l *KVLedger) GetBlockByHash(blockHash []byte) (*common.Block, error) {
	return l.blockStore.RetrieveBlockByHash(blockHash)
}

//Prune prunes the blocks/transactions that satisfy the given policy
func (l *KVLedger) Prune(policy ledger.PrunePolicy) error {
	return errors.New("Not yet implemented")
}

// NewTxSimulator returns new `ledger.TxSimulator`
func (l *KVLedger) NewTxSimulator() (ledger.TxSimulator, error) {
	return l.txtmgmt.NewTxSimulator()
}

// NewQueryExecutor gives handle to a query executer.
// A client can obtain more than one 'QueryExecutor's for parallel execution.
// Any synchronization should be performed at the implementation level if required
func (l *KVLedger) NewQueryExecutor() (ledger.QueryExecutor, error) {
	return l.txtmgmt.NewQueryExecutor()
}

// RemoveInvalidTransactionsAndPrepare validates all the transactions in the given block
// and returns a block that contains only valid transactions and a list of transactions that are invalid
func (l *KVLedger) RemoveInvalidTransactionsAndPrepare(block *common.Block) (*common.Block, []*pb.InvalidTransaction, error) {
	var validBlock *common.Block
	var invalidTxs []*pb.InvalidTransaction
	var err error
	validBlock, invalidTxs, err = l.txtmgmt.ValidateAndPrepare(block, true)
	if err == nil {
		l.pendingBlockToCommit = validBlock
	}
	return validBlock, invalidTxs, err
}

// Commit commits the valid block (returned in the method RemoveInvalidTransactionsAndPrepare) and related state changes
func (l *KVLedger) Commit() error {
	if l.pendingBlockToCommit == nil {
		panic(fmt.Errorf(`Nothing to commit. RemoveInvalidTransactionsAndPrepare() method should have been called and should not have thrown error`))
	}

	logger.Debugf("Committing block to storage")
	if err := l.blockStore.AddBlock(l.pendingBlockToCommit); err != nil {
		return err
	}

	logger.Debugf("Committing block to state database")
	if err := l.txtmgmt.Commit(); err != nil {
		panic(fmt.Errorf(`Error during commit to txmgr:%s`, err))
	}

	//TODO future will want to run async with state db writes.  History needs to wait for chain (FSBlock) to write but not the state db
	logger.Debugf("===HISTORYDB=== Commit() will write to hisotry if enabled else will be by-passed if not enabled: vledgerconfig.IsHistoryDBEnabled(): %v\n", ledgerconfig.IsHistoryDBEnabled())
	if ledgerconfig.IsHistoryDBEnabled() == true {
		logger.Debugf("Committing transactions to history database")
		if err := l.historymgmt.Commit(l.pendingBlockToCommit); err != nil {
			panic(fmt.Errorf(`Error during commit to txthistory:%s`, err))
		}
	}

	l.pendingBlockToCommit = nil
	return nil
}

// Rollback rollbacks the changes caused by the last invocation to method `RemoveInvalidTransactionsAndPrepare`
func (l *KVLedger) Rollback() {
	l.txtmgmt.Rollback()
	l.pendingBlockToCommit = nil
}

// Close closes `KVLedger`
func (l *KVLedger) Close() {
	l.blockStore.Shutdown()
	l.txtmgmt.Shutdown()
}
