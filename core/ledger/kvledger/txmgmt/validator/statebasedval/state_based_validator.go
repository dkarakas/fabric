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

package statebasedval

import (
	"os"
	"time"

	"fmt"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/rwsetutil"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/statedb"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/version"
	"github.com/hyperledger/fabric/core/ledger/util"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/ledger/rwset/kvrwset"
	"github.com/hyperledger/fabric/protos/peer"
	putils "github.com/hyperledger/fabric/protos/utils"
)

var state_based_validator_log, _ = os.Create("/root/state_based_validator.log")

var logger = flogging.MustGetLogger("statevalidator")

// Validator validates a tx against the latest committed state
// and preceding valid transactions with in the same block
type Validator struct {
	db statedb.VersionedDB
}

// NewValidator constructs StateValidator
func NewValidator(db statedb.VersionedDB) *Validator {
	return &Validator{db}
}

//validate endorser transaction
func (v *Validator) validateEndorserTX(envBytes []byte, doMVCCValidation bool, updates *statedb.UpdateBatch) (*rwsetutil.TxRwSet, peer.TxValidationCode, error) {
	// extract actions from the envelope message
	respPayload, err := putils.GetActionFromEnvelope(envBytes)
	if err != nil {
		return nil, peer.TxValidationCode_NIL_TXACTION, nil
	}

	//preparation for extracting RWSet from transaction
	txRWSet := &rwsetutil.TxRwSet{}

	// Get the Result from the Action
	// and then Unmarshal it into a TxReadWriteSet using custom unmarshalling

	if err = txRWSet.FromProtoBytes(respPayload.Results); err != nil {
		return nil, peer.TxValidationCode_INVALID_OTHER_REASON, nil
	}

	txResult := peer.TxValidationCode_VALID

	//mvccvalidation, may invalidate transaction
	if doMVCCValidation {
		if txResult, err = v.validateTx(txRWSet, updates); err != nil {
			return nil, txResult, err
		} else if txResult != peer.TxValidationCode_VALID {
			txRWSet = nil
		}
	}

	return txRWSet, txResult, err
}

func (v *Validator) collectRSetForBlockForBulkOptimizable(blocks []*common.Block) error {
	bulkOptimizable, ok := v.db.(statedb.BulkOptimizable)
	if !ok {
		return nil
	}

	var totalReadSet []*statedb.CompositeKey

	for _, block := range blocks {
		// Committer validator has already set validation flags based on well formed tran checks
		txsFilter := util.TxValidationFlags(block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER])

		// Precaution in case committer validator has not added validation flags yet
		if len(txsFilter) == 0 {
			txsFilter = util.NewTxValidationFlags(len(block.Data.Data))
			block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = txsFilter
		}

		for txIndex, envBytes := range block.Data.Data {
			if txsFilter.IsInvalid(txIndex) {
				// Skiping invalid transaction
				logger.Warningf("Block [%d] Transaction index [%d] marked as invalid by committer. Reason code [%d]",
					block.Header.Number, txIndex, txsFilter.Flag(txIndex))
				continue
			}

			env, err := putils.GetEnvelopeFromBlock(envBytes)
			if err != nil {
				return err
			}

			payload, err := putils.GetPayload(env)
			if err != nil {
				return err
			}

			chdr, err := putils.UnmarshalChannelHeader(payload.Header.ChannelHeader)
			if err != nil {
				return err
			}

			txType := common.HeaderType(chdr.Type)

			if txType != common.HeaderType_ENDORSER_TRANSACTION {
				//			logger.Debugf("Skipping mvcc validation for Block [%d] Transaction index [%d] because, the transaction type is [%s]",
				//				block.Header.Number, txIndex, txType)
				continue
			}

			var txResult peer.TxValidationCode

			// Get the readset
			respPayload, err := putils.GetActionFromEnvelope(envBytes)
			if err != nil {
				txResult = peer.TxValidationCode_NIL_TXACTION
			}
			//preparation for extracting RWSet from transaction
			txRWSet := &rwsetutil.TxRwSet{}
			// Get the Result from the Action
			// and then Unmarshal it into a TxReadWriteSet using custom unmarshalling
			if err = txRWSet.FromProtoBytes(respPayload.Results); err != nil {
				txResult = peer.TxValidationCode_INVALID_OTHER_REASON
			}

			txsFilter.SetFlag(txIndex, txResult)

			//txRWSet != nil => t is valid
			if txRWSet != nil {
				for _, nsRWSet := range txRWSet.NsRwSets {
					for _, kvRead := range nsRWSet.KvRwSet.Reads {
						totalReadSet = append(totalReadSet, &statedb.CompositeKey{
							Namespace: nsRWSet.NameSpace,
							Key:       kvRead.Key,
						})
					}
				}
			}

		}
		block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = txsFilter
	}

	bulkOptimizable.LoadCommittedVersions(totalReadSet)
	return nil
}

// ValidateAndPrepareBatch implements method in Validator interface
func (v *Validator) ValidateAndPrepareBatch(block *common.Block, doMVCCValidation bool) (*statedb.UpdateBatch, error) {
	startTime := time.Now()
	state_based_validator_log.WriteString(fmt.Sprintf("%s ValidateAndPrepareBatch start\n", startTime))
	defer func(startTime time.Time) {
		state_based_validator_log.WriteString(fmt.Sprintf("%s ValidateAndPrepareBatch end %d\n", time.Now(), time.Now().Sub(startTime).Nanoseconds()))
	}(startTime)

	logger.Debugf("New block arrived for validation:%#v, doMVCCValidation=%t", block, doMVCCValidation)
	updates := statedb.NewUpdateBatch()
	logger.Debugf("Validating a block with [%d] transactions", len(block.Data.Data))

	// Committer validator has already set validation flags based on well formed tran checks
	txsFilter := util.TxValidationFlags(block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER])

	// Precaution in case committer validator has not added validation flags yet
	if len(txsFilter) == 0 {
		txsFilter = util.NewTxValidationFlags(len(block.Data.Data))
		block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = txsFilter
	}

	v.collectRSetForBlockForBulkOptimizable([]*common.Block{block})

	for txIndex, envBytes := range block.Data.Data {
		if txsFilter.IsInvalid(txIndex) {
			// Skiping invalid transaction
			logger.Warningf("Block [%d] Transaction index [%d] marked as invalid by committer. Reason code [%d]",
				block.Header.Number, txIndex, txsFilter.Flag(txIndex))
			continue
		}

		sTime := time.Now()
		env, err := putils.GetEnvelopeFromBlock(envBytes)
		state_based_validator_log.WriteString(fmt.Sprintf("%s GetEnvelopeFromBlock done %d\n", time.Now(), time.Now().Sub(sTime).Nanoseconds()))
		if err != nil {
			return nil, err
		}

		payload, err := putils.GetPayload(env)
		if err != nil {
			return nil, err
		}

		chdr, err := putils.UnmarshalChannelHeader(payload.Header.ChannelHeader)
		if err != nil {
			return nil, err
		}

		txType := common.HeaderType(chdr.Type)

		if txType != common.HeaderType_ENDORSER_TRANSACTION {
			logger.Debugf("Skipping mvcc validation for Block [%d] Transaction index [%d] because, the transaction type is [%s]",
				block.Header.Number, txIndex, txType)
			continue
		}

		sTime = time.Now()
		txRWSet, txResult, err := v.validateEndorserTX(envBytes, doMVCCValidation, updates)
		state_based_validator_log.WriteString(fmt.Sprintf("%s validateEndorserTX done %d\n", time.Now(), time.Now().Sub(sTime).Nanoseconds()))

		if err != nil {
			return nil, err
		}

		txsFilter.SetFlag(txIndex, txResult)

		//txRWSet != nil => t is valid
		if txRWSet != nil {
			committingTxHeight := version.NewHeight(block.Header.Number, uint64(txIndex))
			sTime = time.Now()
			addWriteSetToBatch(txRWSet, committingTxHeight, updates)
			state_based_validator_log.WriteString(fmt.Sprintf("%s addWriteSetToBatch done %d\n", time.Now(), time.Now().Sub(sTime).Nanoseconds()))
			txsFilter.SetFlag(txIndex, peer.TxValidationCode_VALID)
		}

		if txsFilter.IsValid(txIndex) {
			logger.Debugf("Block [%d] Transaction index [%d] TxId [%s] marked as valid by state validator",
				block.Header.Number, txIndex, chdr.TxId)
		} else {
			logger.Warningf("Block [%d] Transaction index [%d] TxId [%s] marked as invalid by state validator. Reason code [%d]",
				block.Header.Number, txIndex, chdr.TxId, txsFilter.Flag(txIndex))
		}
	}
	block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = txsFilter
	return updates, nil
}

// ValidateAndPrepareBatchBulk implements method in Validator interface
func (v *Validator) ValidateAndPrepareBatchBulk(blocks []*common.Block, doMVCCValidation bool) (*statedb.UpdateBatch, []error) {
	startTime := time.Now()
	errs := make([]error, len(blocks))

	state_based_validator_log.WriteString(fmt.Sprintf("%s ValidateAndPrepareBatchBulk start len %d\n", startTime, len(blocks)))
	defer func(startTime time.Time) {
		state_based_validator_log.WriteString(fmt.Sprintf("%s ValidateAndPrepareBatchBulk end %d len %d\n", time.Now(), time.Now().Sub(startTime).Nanoseconds(), len(blocks)))
	}(startTime)

	sTime := time.Now()
	v.collectRSetForBlockForBulkOptimizable(blocks)
	state_based_validator_log.WriteString(fmt.Sprintf("%s CollectRSet done %d\n", time.Now(), time.Now().Sub(sTime)))
	updates := statedb.NewUpdateBatch()

	for i, block := range blocks {
		logger.Debugf("New block arrived for validation:%#v, doMVCCValidation=%t", block, doMVCCValidation)
		logger.Debugf("Validating a block with [%d] transactions", len(block.Data.Data))

		// Committer validator has already set validation flags based on well formed tran checks
		txsFilter := util.TxValidationFlags(block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER])

		// Precaution in case committer validator has not added validation flags yet
		if len(txsFilter) == 0 {
			txsFilter = util.NewTxValidationFlags(len(block.Data.Data))
			block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = txsFilter
		}

		for txIndex, envBytes := range block.Data.Data {
			if txsFilter.IsInvalid(txIndex) {
				// Skiping invalid transaction
				logger.Warningf("Block [%d] Transaction index [%d] marked as invalid by committer. Reason code [%d]",
					block.Header.Number, txIndex, txsFilter.Flag(txIndex))
				continue
			}

			sTime := time.Now()
			env, err := putils.GetEnvelopeFromBlock(envBytes)
			state_based_validator_log.WriteString(fmt.Sprintf("%s GetEnvelopeFromBlock done %d\n", time.Now(), time.Now().Sub(sTime).Nanoseconds()))
			if err != nil {
				errs[i] = err
				break // mark the block invalid and continue validating other blocks
			}

			payload, err := putils.GetPayload(env)
			if err != nil {
				errs[i] = err
				break // mark the block invalid and continue validating other blocks
			}

			chdr, err := putils.UnmarshalChannelHeader(payload.Header.ChannelHeader)
			if err != nil {
				errs[i] = err
				break // mark the block invalid and continue validating other blocks
			}

			txType := common.HeaderType(chdr.Type)

			if txType != common.HeaderType_ENDORSER_TRANSACTION {
				logger.Debugf("Skipping mvcc validation for Block [%d] Transaction index [%d] because, the transaction type is [%s]",
					block.Header.Number, txIndex, txType)
				continue
			}

			sTime = time.Now()
			txRWSet, txResult, err := v.validateEndorserTX(envBytes, doMVCCValidation, updates)
			state_based_validator_log.WriteString(fmt.Sprintf("%s validateEndorserTX done %d\n", time.Now(), time.Now().Sub(sTime).Nanoseconds()))

			if err != nil {
				errs[i] = err
				break // mark the block invalid and continue validating other blocks
			}

			txsFilter.SetFlag(txIndex, txResult)

			//txRWSet != nil => t is valid
			if txRWSet != nil {
				committingTxHeight := version.NewHeight(block.Header.Number, uint64(txIndex))
				sTime = time.Now()
				addWriteSetToBatch(txRWSet, committingTxHeight, updates)
				state_based_validator_log.WriteString(fmt.Sprintf("%s addWriteSetToBatch done %d\n", time.Now(), time.Now().Sub(sTime).Nanoseconds()))
				txsFilter.SetFlag(txIndex, peer.TxValidationCode_VALID)
			}

			if txsFilter.IsValid(txIndex) {
				logger.Debugf("Block [%d] Transaction index [%d] TxId [%s] marked as valid by state validator",
					block.Header.Number, txIndex, chdr.TxId)
			} else {
				logger.Warningf("Block [%d] Transaction index [%d] TxId [%s] marked as invalid by state validator. Reason code [%d]",
					block.Header.Number, txIndex, chdr.TxId, txsFilter.Flag(txIndex))
			}
		}

		// if the block is invalid, it'll be ignored by the caller
		block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER] = txsFilter
	}

	return updates, errs
}

func addWriteSetToBatch(txRWSet *rwsetutil.TxRwSet, txHeight *version.Height, batch *statedb.UpdateBatch) {
	for _, nsRWSet := range txRWSet.NsRwSets {
		ns := nsRWSet.NameSpace
		for _, kvWrite := range nsRWSet.KvRwSet.Writes {
			if kvWrite.IsDelete {
				batch.Delete(ns, kvWrite.Key, txHeight)
			} else {
				batch.Put(ns, kvWrite.Key, kvWrite.Value, txHeight)
			}
		}
	}
}

func (v *Validator) validateTx(txRWSet *rwsetutil.TxRwSet, updates *statedb.UpdateBatch) (peer.TxValidationCode, error) {
	for _, nsRWSet := range txRWSet.NsRwSets {
		ns := nsRWSet.NameSpace

		if valid, err := v.validateReadSet(ns, nsRWSet.KvRwSet.Reads, updates); !valid || err != nil {
			if err != nil {
				return peer.TxValidationCode(-1), err
			}
			return peer.TxValidationCode_MVCC_READ_CONFLICT, nil
		}
		if valid, err := v.validateRangeQueries(ns, nsRWSet.KvRwSet.RangeQueriesInfo, updates); !valid || err != nil {
			if err != nil {
				return peer.TxValidationCode(-1), err
			}
			return peer.TxValidationCode_PHANTOM_READ_CONFLICT, nil
		}
	}
	return peer.TxValidationCode_VALID, nil
}

func (v *Validator) validateReadSet(ns string, kvReads []*kvrwset.KVRead, updates *statedb.UpdateBatch) (bool, error) {
	for _, kvRead := range kvReads {
		if valid, err := v.validateKVRead(ns, kvRead, updates); !valid || err != nil {
			return valid, err
		}
	}
	return true, nil
}

// validateKVRead performs mvcc check for a key read during transaction simulation.
// i.e., it checks whether a key/version combination is already updated in the statedb (by an already committed block)
// or in the updates (by a preceding valid transaction in the current block)
func (v *Validator) validateKVRead(ns string, kvRead *kvrwset.KVRead, updates *statedb.UpdateBatch) (bool, error) {
	if updates.Exists(ns, kvRead.Key) {
		return false, nil
	}
	committedVersion, err := v.db.GetVersion(ns, kvRead.Key)
	if err != nil {
		return false, nil
	}

	if !version.AreSame(committedVersion, rwsetutil.NewVersion(kvRead.Version)) {
		logger.Debugf("Version mismatch for key [%s:%s]. Committed version = [%s], Version in readSet [%s]",
			ns, kvRead.Key, committedVersion, kvRead.Version)
		return false, nil
	}
	return true, nil
}

func (v *Validator) validateRangeQueries(ns string, rangeQueriesInfo []*kvrwset.RangeQueryInfo, updates *statedb.UpdateBatch) (bool, error) {
	for _, rqi := range rangeQueriesInfo {
		if valid, err := v.validateRangeQuery(ns, rqi, updates); !valid || err != nil {
			return valid, err
		}
	}
	return true, nil
}

// validateRangeQuery performs a phatom read check i.e., it
// checks whether the results of the range query are still the same when executed on the
// statedb (latest state as of last committed block) + updates (prepared by the writes of preceding valid transactions
// in the current block and yet to be committed as part of group commit at the end of the validation of the block)
func (v *Validator) validateRangeQuery(ns string, rangeQueryInfo *kvrwset.RangeQueryInfo, updates *statedb.UpdateBatch) (bool, error) {
	logger.Debugf("validateRangeQuery: ns=%s, rangeQueryInfo=%s", ns, rangeQueryInfo)

	// If during simulation, the caller had not exhausted the iterator so
	// rangeQueryInfo.EndKey is not actual endKey given by the caller in the range query
	// but rather it is the last key seen by the caller and hence the combinedItr should include the endKey in the results.
	includeEndKey := !rangeQueryInfo.ItrExhausted

	combinedItr, err := newCombinedIterator(v.db, updates,
		ns, rangeQueryInfo.StartKey, rangeQueryInfo.EndKey, includeEndKey)
	if err != nil {
		return false, err
	}
	defer combinedItr.Close()
	var validator rangeQueryValidator
	if rangeQueryInfo.GetReadsMerkleHashes() != nil {
		logger.Debug(`Hashing results are present in the range query info hence, initiating hashing based validation`)
		validator = &rangeQueryHashValidator{}
	} else {
		logger.Debug(`Hashing results are not present in the range query info hence, initiating raw KVReads based validation`)
		validator = &rangeQueryResultsValidator{}
	}
	validator.init(rangeQueryInfo, combinedItr)
	return validator.validate()
}
