// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package batcheval

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/storage/abortspan"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/rditer"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
	"github.com/cockroachdb/cockroach/pkg/storage/stateloader"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/pkg/errors"
)

// TxnAutoGC controls whether Transaction entries are automatically gc'ed
// upon EndTransaction if they only have local intents (which can be
// resolved synchronously with EndTransaction). Certain tests become
// simpler with this being turned off.
var TxnAutoGC = true

func init() {
	RegisterCommand(roachpb.EndTransaction, declareKeysEndTransaction, evalEndTransaction)
}

func declareKeysEndTransaction(
	desc roachpb.RangeDescriptor, header roachpb.Header, req roachpb.Request, spans *spanset.SpanSet,
) {
	declareKeysWriteTransaction(desc, header, req, spans)
	et := req.(*roachpb.EndTransactionRequest)
	// The spans may extend beyond this Range, but it's ok for the
	// purpose of the command queue. The parts in our Range will
	// be resolved eagerly.
	for _, span := range et.IntentSpans {
		spans.Add(spanset.SpanReadWrite, span)
	}
	if header.Txn != nil {
		header.Txn.AssertInitialized(context.TODO())
		abortSpanAccess := spanset.SpanReadOnly
		if !et.Commit && et.Poison {
			abortSpanAccess = spanset.SpanReadWrite
		}
		spans.Add(abortSpanAccess, roachpb.Span{
			Key: keys.AbortSpanKey(header.RangeID, header.Txn.ID),
		})
	}

	// All transactions depend on the range descriptor because they need
	// to determine which intents are within the local range.
	spans.Add(spanset.SpanReadOnly, roachpb.Span{Key: keys.RangeDescriptorKey(desc.StartKey)})

	if et.InternalCommitTrigger != nil {
		if st := et.InternalCommitTrigger.SplitTrigger; st != nil {
			// Splits may read from the entire pre-split range (they read
			// from the LHS in all cases, and the RHS only when the existing
			// stats contain estimates), but they need to declare a write
			// access to block all other concurrent writes. We block writes
			// to the RHS because they will fail if applied after the split,
			// and writes to the LHS because their stat deltas will
			// interfere with the non-delta stats computed as a part of the
			// split. (see
			// https://github.com/cockroachdb/cockroach/issues/14881)
			spans.Add(spanset.SpanReadWrite, roachpb.Span{
				Key:    st.LeftDesc.StartKey.AsRawKey(),
				EndKey: st.RightDesc.EndKey.AsRawKey(),
			})
			spans.Add(spanset.SpanReadWrite, roachpb.Span{
				Key:    keys.MakeRangeKeyPrefix(st.LeftDesc.StartKey),
				EndKey: keys.MakeRangeKeyPrefix(st.RightDesc.EndKey).PrefixEnd(),
			})
			leftRangeIDPrefix := keys.MakeRangeIDReplicatedPrefix(header.RangeID)
			spans.Add(spanset.SpanReadOnly, roachpb.Span{
				Key:    leftRangeIDPrefix,
				EndKey: leftRangeIDPrefix.PrefixEnd(),
			})

			rightRangeIDPrefix := keys.MakeRangeIDReplicatedPrefix(st.RightDesc.RangeID)
			spans.Add(spanset.SpanReadWrite, roachpb.Span{
				Key:    rightRangeIDPrefix,
				EndKey: rightRangeIDPrefix.PrefixEnd(),
			})
			rightRangeIDUnreplicatedPrefix := keys.MakeRangeIDUnreplicatedPrefix(st.RightDesc.RangeID)
			spans.Add(spanset.SpanReadWrite, roachpb.Span{
				Key:    rightRangeIDUnreplicatedPrefix,
				EndKey: rightRangeIDUnreplicatedPrefix.PrefixEnd(),
			})

			spans.Add(spanset.SpanReadOnly, roachpb.Span{
				Key: keys.RangeLastReplicaGCTimestampKey(st.LeftDesc.RangeID),
			})
			spans.Add(spanset.SpanReadWrite, roachpb.Span{
				Key: keys.RangeLastReplicaGCTimestampKey(st.RightDesc.RangeID),
			})

			spans.Add(spanset.SpanReadOnly, roachpb.Span{
				Key:    abortspan.MinKey(header.RangeID),
				EndKey: abortspan.MaxKey(header.RangeID)})
		}
		if mt := et.InternalCommitTrigger.MergeTrigger; mt != nil {
			// Merges write to the left side's abort span and the right side's data
			// and range-local spans. They also read from the right side's range ID
			// span.
			leftRangeIDPrefix := keys.MakeRangeIDReplicatedPrefix(header.RangeID)
			spans.Add(spanset.SpanReadWrite, roachpb.Span{
				Key:    leftRangeIDPrefix,
				EndKey: leftRangeIDPrefix.PrefixEnd(),
			})
			spans.Add(spanset.SpanReadWrite, roachpb.Span{
				Key:    mt.RightDesc.StartKey.AsRawKey(),
				EndKey: mt.RightDesc.EndKey.AsRawKey(),
			})
			spans.Add(spanset.SpanReadWrite, roachpb.Span{
				Key:    keys.MakeRangeKeyPrefix(mt.RightDesc.StartKey),
				EndKey: keys.MakeRangeKeyPrefix(mt.RightDesc.EndKey),
			})
			spans.Add(spanset.SpanReadOnly, roachpb.Span{
				Key:    keys.MakeRangeIDReplicatedPrefix(mt.RightDesc.RangeID),
				EndKey: keys.MakeRangeIDReplicatedPrefix(mt.RightDesc.RangeID).PrefixEnd(),
			})
		}
	}
}

// evalEndTransaction either commits or aborts (rolls back) an extant
// transaction according to the args.Commit parameter. Rolling back
// an already rolled-back txn is ok.
func evalEndTransaction(
	ctx context.Context, batch engine.ReadWriter, cArgs CommandArgs, resp roachpb.Response,
) (result.Result, error) {
	args := cArgs.Args.(*roachpb.EndTransactionRequest)
	h := cArgs.Header
	ms := cArgs.Stats
	reply := resp.(*roachpb.EndTransactionResponse)

	if err := VerifyTransaction(h, args); err != nil {
		return result.Result{}, err
	}

	// If a 1PC txn was required and we're in EndTransaction, something went wrong.
	if args.Require1PC {
		return result.Result{}, roachpb.NewTransactionStatusError("could not commit in one phase as requested")
	}

	key := keys.TransactionKey(h.Txn.Key, h.Txn.ID)

	// Fetch existing transaction.
	var existingTxn roachpb.Transaction
	if ok, err := engine.MVCCGetProto(
		ctx, batch, key, hlc.Timestamp{}, true /* consistent */, nil /* txn */, &existingTxn,
	); err != nil {
		return result.Result{}, err
	} else if !ok {
		if args.Commit {
			return result.Result{}, roachpb.NewTransactionNotFoundStatusError()
		}
		// For rollbacks, we don't consider not finding the txn record an error;
		// we accept without fuss rollbacks for transactions where the txn record
		// was never written (because the BeginTransaction's batch failed, or
		// even where the BeginTransaction was never sent to server).
		txn := h.Txn.Clone()
		reply.Txn = &txn
		return result.Result{}, nil
	}
	// We're using existingTxn on the reply, although it can be stale
	// compared to the Transaction in the request (e.g. the Sequence,
	// and various timestamps). We must be careful to update it with the
	// supplied ba.Txn if we return it with an error which might be
	// retried, as for example to avoid client-side serializable restart.
	reply.Txn = &existingTxn

	// Verify that we can either commit it or abort it (according
	// to args.Commit), and also that the Timestamp and Epoch have
	// not suffered regression.
	switch reply.Txn.Status {
	case roachpb.COMMITTED:
		return result.Result{}, roachpb.NewTransactionCommittedStatusError()

	case roachpb.ABORTED:
		if !args.Commit {
			// The transaction has already been aborted by other.
			// Do not return TransactionAbortedError since the client anyway
			// wanted to abort the transaction.
			desc := cArgs.EvalCtx.Desc()
			externalIntents, err := resolveLocalIntents(ctx, desc, batch, ms, *args, reply.Txn, cArgs.EvalCtx)
			if err != nil {
				return result.Result{}, err
			}
			if err := updateTxnWithExternalIntents(
				ctx, batch, ms, *args, reply.Txn, externalIntents,
			); err != nil {
				return result.Result{}, err
			}
			// Use alwaysReturn==true because the transaction is definitely
			// aborted, no matter what happens to this command.
			return result.FromEndTxn(reply.Txn, true /* alwaysReturn */, args.Poison), nil
		}
		// If the transaction was previously aborted by a concurrent writer's
		// push, any intents written are still open. It's only now that we know
		// them, so we return them all for asynchronous resolution (we're
		// currently not able to write on error, but see #1989).
		//
		// Similarly to above, use alwaysReturn==true. The caller isn't trying
		// to abort, but the transaction is definitely aborted and its intents
		// can go.
		reply.Txn.Intents = args.IntentSpans
		return result.FromEndTxn(reply.Txn, true /* alwaysReturn */, args.Poison),
			roachpb.NewTransactionAbortedError(roachpb.ABORT_REASON_ABORTED_RECORD_FOUND)

	case roachpb.PENDING:
		if h.Txn.Epoch < reply.Txn.Epoch {
			// TODO(tschottdorf): this leaves the Txn record (and more
			// importantly, intents) dangling; we can't currently write on
			// error. Would panic, but that makes TestEndTransactionWithErrors
			// awkward.
			return result.Result{}, roachpb.NewTransactionStatusError(
				fmt.Sprintf("epoch regression: %d", h.Txn.Epoch),
			)
		} else if h.Txn.Epoch == reply.Txn.Epoch && reply.Txn.Timestamp.Less(h.Txn.OrigTimestamp) {
			// The transaction record can only ever be pushed forward, so it's an
			// error if somehow the transaction record has an earlier timestamp
			// than the original transaction timestamp.

			// TODO(tschottdorf): see above comment on epoch regression.
			return result.Result{}, roachpb.NewTransactionStatusError(
				fmt.Sprintf("timestamp regression: %s", h.Txn.OrigTimestamp),
			)
		}

	default:
		return result.Result{}, roachpb.NewTransactionStatusError(
			fmt.Sprintf("bad txn status: %s", reply.Txn),
		)
	}

	// Update the existing txn with the supplied txn.
	reply.Txn.Update(h.Txn)

	var pd result.Result

	// Set transaction status to COMMITTED or ABORTED as per the
	// args.Commit parameter.
	if args.Commit {
		if retry, reason := IsEndTransactionTriggeringRetryError(reply.Txn, *args); retry {
			return result.Result{}, roachpb.NewTransactionRetryError(reason)
		}

		if IsEndTransactionExceedingDeadline(reply.Txn.Timestamp, *args) {
			// If the deadline has lapsed return an error and rely on the client
			// issuing a Rollback() that aborts the transaction and cleans up
			// intents. Unfortunately, we're returning an error and unable to
			// write on error (see #1989): we can't write ABORTED into the master
			// transaction record which remains PENDING, and thus rely on the
			// client to issue a Rollback() for cleanup.
			//
			// N.B. This deadline test is expected to be a Noop for Serializable
			// transactions; unless the client misconfigured the txn, the deadline can
			// only be expired if the txn has been pushed, and pushed Serializable
			// transactions are detected above.
			return result.Result{}, roachpb.NewTransactionStatusError(
				"transaction deadline exceeded")
		}

		reply.Txn.Status = roachpb.COMMITTED

		// Merge triggers must run before intent resolution as the merge trigger
		// itself contains intents, in the RightData snapshot, that will be owned
		// and thus resolved by the new range.
		//
		// While it might seem cleaner to simply rely on asynchronous intent
		// resolution here, these intents must be resolved synchronously. We
		// maintain the invariant that there are no intents on local range
		// descriptors that belong to committed transactions. This allows nodes,
		// during startup, to infer that any lingering intents belong to in-progress
		// transactions and thus the pre-intent value can safely be used.
		if mt := args.InternalCommitTrigger.GetMergeTrigger(); mt != nil {
			mergeResult, err := mergeTrigger(ctx, cArgs.EvalCtx, batch.(engine.Batch),
				ms, mt, reply.Txn.Timestamp)
			if err != nil {
				return result.Result{}, err
			}
			if err := pd.MergeAndDestroy(mergeResult); err != nil {
				return result.Result{}, err
			}
		}
	} else {
		reply.Txn.Status = roachpb.ABORTED
	}

	desc := cArgs.EvalCtx.Desc()
	externalIntents, err := resolveLocalIntents(ctx, desc, batch, ms, *args, reply.Txn, cArgs.EvalCtx)
	if err != nil {
		return result.Result{}, err
	}
	if err := updateTxnWithExternalIntents(ctx, batch, ms, *args, reply.Txn, externalIntents); err != nil {
		return result.Result{}, err
	}

	// Run triggers if successfully committed.
	if reply.Txn.Status == roachpb.COMMITTED {
		triggerResult, err := RunCommitTrigger(ctx, cArgs.EvalCtx, batch.(engine.Batch),
			ms, *args, reply.Txn)
		if err != nil {
			return result.Result{}, roachpb.NewReplicaCorruptionError(err)
		}
		if err := pd.MergeAndDestroy(triggerResult); err != nil {
			return result.Result{}, err
		}
	}

	// Note: there's no need to clear the AbortSpan state if we've
	// successfully finalized a transaction, as there's no way in which an abort
	// cache entry could have been written (the txn would already have been in
	// state=ABORTED).
	//
	// Summary of transaction replay protection after EndTransaction: When a
	// transactional write gets replayed over its own resolved intents, the
	// write will succeed but only as an intent with a newer timestamp (with a
	// WriteTooOldError). However, the replayed intent cannot be resolved by a
	// subsequent replay of this EndTransaction call because the txn timestamp
	// will be too old. Replays which include a BeginTransaction never succeed
	// because EndTransaction inserts in the write timestamp cache, forcing the
	// BeginTransaction to fail with a transaction retry error. If the replay
	// didn't include a BeginTransaction, any push will immediately succeed as a
	// missing txn record on push sets the transaction to aborted. In both
	// cases, the txn will be GC'd on the slow path.
	//
	// We specify alwaysReturn==false because if the commit fails below Raft, we
	// don't want the intents to be up for resolution. That should happen only
	// if the commit actually happens; otherwise, we risk losing writes.
	intentsResult := result.FromEndTxn(reply.Txn, false /* alwaysReturn */, args.Poison)
	intentsResult.Local.UpdatedTxns = &[]*roachpb.Transaction{reply.Txn}
	if err := pd.MergeAndDestroy(intentsResult); err != nil {
		return result.Result{}, err
	}
	return pd, nil
}

// IsEndTransactionExceedingDeadline returns true if the transaction
// exceeded its deadline.
func IsEndTransactionExceedingDeadline(t hlc.Timestamp, args roachpb.EndTransactionRequest) bool {
	return args.Deadline != nil && args.Deadline.Less(t)
}

// IsEndTransactionTriggeringRetryError returns true if the
// EndTransactionRequest cannot be committed and needs to return a
// TransactionRetryError.
func IsEndTransactionTriggeringRetryError(
	txn *roachpb.Transaction, args roachpb.EndTransactionRequest,
) (retry bool, reason roachpb.TransactionRetryReason) {
	// If we saw any WriteTooOldErrors, we must restart to avoid lost
	// update anomalies.
	if txn.WriteTooOld {
		retry, reason = true, roachpb.RETRY_WRITE_TOO_OLD
	} else {
		origTimestamp := txn.OrigTimestamp
		origTimestamp.Forward(txn.RefreshedTimestamp)
		isTxnPushed := txn.Timestamp != origTimestamp

		// If the isolation level is SERIALIZABLE, return a transaction
		// retry error if the commit timestamp isn't equal to the txn
		// timestamp.
		if isTxnPushed {
			if txn.Isolation == enginepb.SERIALIZABLE {
				retry, reason = true, roachpb.RETRY_SERIALIZABLE
			} else if txn.RetryOnPush {
				// If pushing requires a retry and the transaction was pushed, retry.
				retry, reason = true, roachpb.RETRY_DELETE_RANGE
			}
		}
	}

	// A serializable transaction can still avoid a retry under certain conditions.
	if retry && txn.IsSerializable() && canForwardSerializableTimestamp(txn, args.NoRefreshSpans) {
		retry, reason = false, 0
	}

	return retry, reason
}

// canForwardSerializableTimestamp returns whether a serializable txn can
// be safely committed with a forwarded timestamp. This requires that
// the transaction's timestamp has not leaked and that the transaction
// has encountered no spans which require refreshing at the forwarded
// timestamp. If either of those conditions are true, a client-side
// retry is required.
func canForwardSerializableTimestamp(txn *roachpb.Transaction, noRefreshSpans bool) bool {
	return !txn.OrigTimestampWasObserved && noRefreshSpans
}

const intentResolutionBatchSize = 500

// resolveLocalIntents synchronously resolves any intents that are
// local to this range in the same batch. The remainder are collected
// and returned so that they can be handed off to asynchronous
// processing. Note that there is a maximum intent resolution
// allowance of intentResolutionBatchSize meant to avoid creating a
// batch which is too large for Raft. Any local intents which exceed
// the allowance are treated as external and are resolved
// asynchronously with the external intents.
func resolveLocalIntents(
	ctx context.Context,
	desc *roachpb.RangeDescriptor,
	batch engine.ReadWriter,
	ms *enginepb.MVCCStats,
	args roachpb.EndTransactionRequest,
	txn *roachpb.Transaction,
	evalCtx EvalContext,
) ([]roachpb.Span, error) {
	if mergeTrigger := args.InternalCommitTrigger.GetMergeTrigger(); mergeTrigger != nil {
		// If this is a merge, then use the post-merge descriptor to determine
		// which intents are local (note that for a split, we want to use the
		// pre-split one instead because it's larger).
		desc = &mergeTrigger.LeftDesc
	}

	min, max := txn.InclusiveTimeBounds()
	iter := batch.NewIterator(engine.IterOptions{
		MinTimestampHint: min,
		MaxTimestampHint: max,
		UpperBound:       desc.EndKey.AsRawKey(),
	})
	iterAndBuf := engine.GetBufUsingIter(iter)
	defer iterAndBuf.Cleanup()

	var externalIntents []roachpb.Span
	var resolveAllowance int64 = intentResolutionBatchSize
	for _, span := range args.IntentSpans {
		if err := func() error {
			if resolveAllowance == 0 {
				externalIntents = append(externalIntents, span)
				return nil
			}
			intent := roachpb.Intent{Span: span, Txn: txn.TxnMeta, Status: txn.Status}
			if len(span.EndKey) == 0 {
				// For single-key intents, do a KeyAddress-aware check of
				// whether it's contained in our Range.
				if !storagebase.ContainsKey(*desc, span.Key) {
					externalIntents = append(externalIntents, span)
					return nil
				}
				resolveMS := ms
				resolveAllowance--
				return engine.MVCCResolveWriteIntentUsingIter(ctx, batch, iterAndBuf, resolveMS, intent)
			}
			// For intent ranges, cut into parts inside and outside our key
			// range. Resolve locally inside, delegate the rest. In particular,
			// an intent range for range-local data is correctly considered local.
			inSpan, outSpans := storagebase.IntersectSpan(span, *desc)
			externalIntents = append(externalIntents, outSpans...)
			if inSpan != nil {
				intent.Span = *inSpan
				num, resumeSpan, err := engine.MVCCResolveWriteIntentRangeUsingIter(ctx, batch, iterAndBuf, ms, intent, resolveAllowance)
				if err != nil {
					return err
				}
				if evalCtx.EvalKnobs().NumKeysEvaluatedForRangeIntentResolution != nil {
					atomic.AddInt64(evalCtx.EvalKnobs().NumKeysEvaluatedForRangeIntentResolution, num)
				}
				resolveAllowance -= num
				if resumeSpan != nil {
					if resolveAllowance != 0 {
						log.Fatalf(ctx, "expected resolve allowance to be exactly 0 resolving %s; got %d", intent.Span, resolveAllowance)
					}
					externalIntents = append(externalIntents, *resumeSpan)
				}
				return nil
			}
			return nil
		}(); err != nil {
			// TODO(tschottdorf): any legitimate reason for this to happen?
			// Figure that out and if not, should still be ReplicaCorruption
			// and not a panic.
			panic(fmt.Sprintf("error resolving intent at %s on end transaction [%s]: %s", span, txn.Status, err))
		}
	}
	// If the poison arg is set, make sure to set the abort span entry.
	if args.Poison && txn.Status == roachpb.ABORTED {
		if err := SetAbortSpan(ctx, evalCtx, batch, ms, txn.TxnMeta, true /* poison */); err != nil {
			return nil, err
		}
	}

	return externalIntents, nil
}

// updateTxnWithExternalIntents persists the transaction record with
// updated status (& possibly timestamp). If we've already resolved
// all intents locally, we actually delete the record right away - no
// use in keeping it around.
func updateTxnWithExternalIntents(
	ctx context.Context,
	batch engine.ReadWriter,
	ms *enginepb.MVCCStats,
	args roachpb.EndTransactionRequest,
	txn *roachpb.Transaction,
	externalIntents []roachpb.Span,
) error {
	key := keys.TransactionKey(txn.Key, txn.ID)
	if TxnAutoGC && len(externalIntents) == 0 {
		if log.V(2) {
			log.Infof(ctx, "auto-gc'ed %s (%d intents)", txn.Short(), len(args.IntentSpans))
		}
		return engine.MVCCDelete(ctx, batch, ms, key, hlc.Timestamp{}, nil /* txn */)
	}
	txn.Intents = externalIntents
	return engine.MVCCPutProto(ctx, batch, ms, key, hlc.Timestamp{}, nil /* txn */, txn)
}

// RunCommitTrigger runs the commit trigger from an end transaction request.
func RunCommitTrigger(
	ctx context.Context,
	rec EvalContext,
	batch engine.Batch,
	ms *enginepb.MVCCStats,
	args roachpb.EndTransactionRequest,
	txn *roachpb.Transaction,
) (result.Result, error) {
	ct := args.InternalCommitTrigger
	if ct == nil {
		return result.Result{}, nil
	}

	if ct.GetSplitTrigger() != nil {
		newMS, trigger, err := splitTrigger(
			ctx, rec, batch, *ms, ct.SplitTrigger, txn.Timestamp,
		)
		*ms = newMS
		return trigger, err
	}
	if crt := ct.GetChangeReplicasTrigger(); crt != nil {
		return changeReplicasTrigger(ctx, rec, batch, crt), nil
	}
	if ct.GetModifiedSpanTrigger() != nil {
		var pd result.Result
		if ct.ModifiedSpanTrigger.SystemConfigSpan {
			// Check if we need to gossip the system config.
			// NOTE: System config gossiping can only execute correctly if
			// the transaction record is located on the range that contains
			// the system span. If a transaction is created which modifies
			// both system *and* non-system data, it should be ensured that
			// the transaction record itself is on the system span. This can
			// be done by making sure a system key is the first key touched
			// in the transaction.
			if rec.ContainsKey(keys.SystemConfigSpan.Key) {
				if err := pd.MergeAndDestroy(
					result.Result{
						Local: result.LocalResult{
							MaybeGossipSystemConfig: true,
						},
					},
				); err != nil {
					return result.Result{}, err
				}
			} else {
				log.Errorf(ctx, "System configuration span was modified, but the "+
					"modification trigger is executing on a non-system range. "+
					"Configuration changes will not be gossiped.")
			}
		}
		if nlSpan := ct.ModifiedSpanTrigger.NodeLivenessSpan; nlSpan != nil {
			if err := pd.MergeAndDestroy(
				result.Result{
					Local: result.LocalResult{
						MaybeGossipNodeLiveness: nlSpan,
					},
				},
			); err != nil {
				return result.Result{}, err
			}
		}
		return pd, nil
	}
	if ct.GetMergeTrigger() != nil {
		// Merge triggers were handled earlier, before intent resolution.
		return result.Result{}, nil
	}

	log.Fatalf(ctx, "unknown commit trigger: %+v", ct)
	return result.Result{}, nil
}

// splitTrigger is called on a successful commit of a transaction
// containing an AdminSplit operation. It copies the AbortSpan for
// the new range and recomputes stats for both the existing, left hand
// side (LHS) range and the right hand side (RHS) range. For
// performance it only computes the stats for the original range (the
// left hand side) and infers the RHS stats by subtracting from the
// original stats. We compute the LHS stats because the split key
// computation ensures that we do not create large LHS
// ranges. However, this optimization is only possible if the stats
// are fully accurate. If they contain estimates, stats for both the
// LHS and RHS are computed.
//
// Splits are complicated. A split is initiated when a replica receives an
// AdminSplit request. Note that this request (and other "admin" requests)
// differs from normal requests in that it doesn't go through Raft but instead
// allows the lease holder Replica to act as the orchestrator for the
// distributed transaction that performs the split. As such, this request is
// only executed on the lease holder replica and the request is redirected to
// the lease holder if the recipient is a follower.
//
// Splits do not require the lease for correctness (which is good, because we
// only check that the lease is held at the beginning of the operation, and
// have no way to ensure that it is continually held until the end). Followers
// could perform splits too, and the only downside would be that if two splits
// were attempted concurrently (or a split and a ChangeReplicas), one would
// fail. The lease is used to designate one replica for this role and avoid
// wasting time on splits that may fail.
//
// The processing of splits is divided into two phases. The first phase occurs
// in Replica.AdminSplit. In that phase, the split-point is computed, and a
// transaction is started which updates both the LHS and RHS range descriptors
// and the meta range addressing information. (If we're splitting a meta2 range
// we'll be updating the meta1 addressing, otherwise we'll be updating the
// meta2 addressing). That transaction includes a special SplitTrigger flag on
// the EndTransaction request. Like all transactions, the requests within the
// transaction are replicated via Raft, including the EndTransaction request.
//
// The second phase of split processing occurs when each replica for the range
// encounters the SplitTrigger. Processing of the SplitTrigger happens below,
// in Replica.splitTrigger. The processing of the SplitTrigger occurs in two
// stages. The first stage operates within the context of an engine.Batch and
// updates all of the on-disk state for the old and new ranges atomically. The
// second stage is invoked when the batch commits and updates the in-memory
// state, creating the new replica in memory and populating its timestamp cache
// and registering it with the store.
//
// There is lots of subtlety here. The easy scenario is that all of the
// replicas process the SplitTrigger before processing any Raft message for RHS
// (right hand side) of the newly split range. Something like:
//
//         Node A             Node B             Node C
//     ----------------------------------------------------
// range 1   |                  |                  |
//           |                  |                  |
//      SplitTrigger            |                  |
//           |             SplitTrigger            |
//           |                  |             SplitTrigger
//           |                  |                  |
//     ----------------------------------------------------
// split finished on A, B and C |                  |
//           |                  |                  |
// range 2   |                  |                  |
//           | ---- MsgVote --> |                  |
//           | ---------------------- MsgVote ---> |
//
// But that ideal ordering is not guaranteed. The split is "finished" when two
// of the replicas have appended the end-txn request containing the
// SplitTrigger to their Raft log. The following scenario is possible:
//
//         Node A             Node B             Node C
//     ----------------------------------------------------
// range 1   |                  |                  |
//           |                  |                  |
//      SplitTrigger            |                  |
//           |             SplitTrigger            |
//           |                  |                  |
//     ----------------------------------------------------
// split finished on A and B    |                  |
//           |                  |                  |
// range 2   |                  |                  |
//           | ---- MsgVote --> |                  |
//           | --------------------- MsgVote ---> ???
//           |                  |                  |
//           |                  |             SplitTrigger
//
// In this scenario, C will create range 2 upon reception of the MsgVote from
// A, though locally that span of keys is still part of range 1. This is
// possible because at the Raft level ranges are identified by integer IDs and
// it isn't until C receives a snapshot of range 2 from the leader that it
// discovers the span of keys it covers. In order to prevent C from fully
// initializing range 2 in this instance, we prohibit applying a snapshot to a
// range if the snapshot overlaps another range. See Store.canApplySnapshotLocked.
//
// But while a snapshot may not have been applied at C, an uninitialized
// Replica was created. An uninitialized Replica is one which belongs to a Raft
// group but for which the range descriptor has not been received. This Replica
// will have participated in the Raft elections. When we're creating the new
// Replica below we take control of this uninitialized Replica and stop it from
// responding to Raft messages by marking it "destroyed". Note that we use the
// Replica.mu.destroyed field for this, but we don't do everything that
// Replica.Destroy does (so we should probably rename that field in light of
// its new uses). In particular we don't touch any data on disk or leave a
// tombstone. This is especially important because leaving a tombstone would
// prevent the legitimate recreation of this replica.
//
// There is subtle synchronization here that is currently controlled by the
// Store.processRaft goroutine. In particular, the serial execution of
// Replica.handleRaftReady by Store.processRaft ensures that an uninitialized
// RHS won't be concurrently executing in Replica.handleRaftReady because we're
// currently running on that goroutine (i.e. Replica.splitTrigger is called on
// the processRaft goroutine).
//
// TODO(peter): The above synchronization needs to be fixed. Using a single
// goroutine for executing Replica.handleRaftReady is undesirable from a
// performance perspective. Likely we will have to add a mutex to Replica to
// protect handleRaftReady and to grab that mutex below when marking the
// uninitialized Replica as "destroyed". Hopefully we'll also be able to remove
// Store.processRaftMu.
//
// Note that in this more complex scenario, A (which performed the SplitTrigger
// first) will create the associated Raft group for range 2 and start
// campaigning immediately. It is possible for B to receive MsgVote requests
// before it has applied the SplitTrigger as well. Both B and C will vote for A
// (and preserve the records of that vote in their HardState). It is critically
// important for Raft correctness that we do not lose the records of these
// votes. After electing A the Raft leader for range 2, A will then attempt to
// send a snapshot to B and C and we'll fall into the situation above where a
// snapshot is received for a range before it has finished splitting from its
// sibling and is thus rejected. An interesting subtlety here: A will send a
// snapshot to B and C because when range 2 is initialized we were careful set
// synthesize its HardState to set its Raft log index to 10. If we had instead
// used log index 0, Raft would have believed the group to be empty, but the
// RHS has something. Using a non-zero initial log index causes Raft to believe
// that there is a discarded prefix to the log and will thus send a snapshot to
// followers.
//
// A final point of clarification: when we split a range we're splitting the
// data the range contains. But we're not forking or splitting the associated
// Raft group. Instead, we're creating a new Raft group to control the RHS of
// the split. That Raft group is starting from an empty Raft log (positioned at
// log entry 10) and a snapshot of the RHS of the split range.
//
// After the split trigger returns, the on-disk state of the right-hand side
// will be suitable for instantiating the right hand side Replica, and
// a suitable trigger is returned, along with the updated stats which represent
// the LHS delta caused by the split (i.e. all writes in the current batch
// which went to the left-hand side, minus the kv pairs which moved to the
// RHS).
//
// These stats are suitable for returning up the callstack like those for
// regular commands; the corresponding delta for the RHS is part of the
// returned trigger and is handled by the Store.
func splitTrigger(
	ctx context.Context,
	rec EvalContext,
	batch engine.Batch,
	bothDeltaMS enginepb.MVCCStats,
	split *roachpb.SplitTrigger,
	ts hlc.Timestamp,
) (enginepb.MVCCStats, result.Result, error) {
	// TODO(tschottdorf): should have an incoming context from the corresponding
	// EndTransaction, but the plumbing has not been done yet.
	sp := rec.Tracer().StartSpan("split", tracing.LogTagsFromCtx(ctx))
	defer sp.Finish()
	desc := rec.Desc()
	if !bytes.Equal(desc.StartKey, split.LeftDesc.StartKey) ||
		!bytes.Equal(desc.EndKey, split.RightDesc.EndKey) {
		return enginepb.MVCCStats{}, result.Result{}, errors.Errorf("range does not match splits: (%s-%s) + (%s-%s) != %s",
			split.LeftDesc.StartKey, split.LeftDesc.EndKey,
			split.RightDesc.StartKey, split.RightDesc.EndKey, rec)
	}

	// Preserve stats for pre-split range, excluding the current batch.
	origBothMS := rec.GetMVCCStats()

	// TODO(d4l3k): we should check which side of the split is smaller
	// and compute stats for it instead of having a constraint that the
	// left hand side is smaller.

	// Compute (absolute) stats for LHS range. Don't write to the LHS below;
	// this needs to happen before this step.
	leftMS, err := rditer.ComputeStatsForRange(&split.LeftDesc, batch, ts.WallTime)
	if err != nil {
		return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to compute stats for LHS range after split")
	}
	log.Event(ctx, "computed stats for left hand side range")

	// Copy the last replica GC timestamp. This value is unreplicated,
	// which is why the MVCC stats are set to nil on calls to
	// MVCCPutProto.
	replicaGCTS, err := rec.GetLastReplicaGCTimestamp(ctx)
	if err != nil {
		return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to fetch last replica GC timestamp")
	}
	if err := engine.MVCCPutProto(ctx, batch, nil, keys.RangeLastReplicaGCTimestampKey(split.RightDesc.RangeID), hlc.Timestamp{}, nil, &replicaGCTS); err != nil {
		return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to copy last replica GC timestamp")
	}

	// Initialize the RHS range's AbortSpan by copying the LHS's.
	if err := rec.AbortSpan().CopyTo(
		ctx, batch, batch, &bothDeltaMS, ts, split.RightDesc.RangeID,
	); err != nil {
		return enginepb.MVCCStats{}, result.Result{}, err
	}

	// Compute (absolute) stats for RHS range.
	var rightMS enginepb.MVCCStats
	if origBothMS.ContainsEstimates || bothDeltaMS.ContainsEstimates {
		// Because either the original stats or the delta stats contain
		// estimate values, we cannot perform arithmetic to determine the
		// new range's stats. Instead, we must recompute by iterating
		// over the keys and counting.
		rightMS, err = rditer.ComputeStatsForRange(&split.RightDesc, batch, ts.WallTime)
		if err != nil {
			return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to compute stats for RHS range after split")
		}
	} else {
		// Because neither the original stats nor the delta stats contain
		// estimate values, we can safely perform arithmetic to determine the
		// new range's stats. The calculation looks like:
		//   rhs_ms = orig_both_ms - orig_left_ms + right_delta_ms
		//          = orig_both_ms - left_ms + left_delta_ms + right_delta_ms
		//          = orig_both_ms - left_ms + delta_ms
		// where the following extra helper variables are used:
		// - orig_left_ms: the left-hand side key range, before the split
		// - (left|right)_delta_ms: the contributions to bothDeltaMS in this batch,
		//   itemized by the side of the split.
		//
		// Note that the result of that computation never has ContainsEstimates
		// set due to none of the inputs having it.

		// Start with the full stats before the split.
		rightMS = origBothMS
		// Remove stats from the left side of the split, at the same time adding
		// the batch contributions for the right-hand side.
		rightMS.Subtract(leftMS)
		rightMS.Add(bothDeltaMS)
	}

	// Note: we don't copy the queue last processed times. This means
	// we'll process the RHS range in consistency and time series
	// maintenance queues again possibly sooner than if we copied. The
	// intent is to limit post-raft logic.

	// Now that we've computed the stats for the RHS so far, we persist them.
	// This looks a bit more complicated than it really is: updating the stats
	// also changes the stats, and we write not only the stats but a complete
	// initial state. Additionally, since bothDeltaMS is tracking writes to
	// both sides, we need to update it as well.
	{
		preRightMS := rightMS // for bothDeltaMS

		// Various pieces of code rely on a replica's lease never being unitialized,
		// but it's more than that - it ensures that we properly initialize the
		// timestamp cache, which is only populated on the lease holder, from that
		// of the original Range.  We found out about a regression here the hard way
		// in #7899. Prior to this block, the following could happen:
		// - a client reads key 'd', leaving an entry in the timestamp cache on the
		//   lease holder of [a,e) at the time, node one.
		// - the range [a,e) splits at key 'c'. [c,e) starts out without a lease.
		// - the replicas of [a,e) on nodes one and two both process the split
		//   trigger and thus copy their timestamp caches to the new right-hand side
		//   Replica. However, only node one's timestamp cache contains information
		//   about the read of key 'd' in the first place.
		// - node two becomes the lease holder for [c,e). Its timestamp cache does
		//   not know about the read at 'd' which happened at the beginning.
		// - node two can illegally propose a write to 'd' at a lower timestamp.
		//
		// TODO(tschottdorf): why would this use r.store.Engine() and not the
		// batch?
		leftLease, err := MakeStateLoader(rec).LoadLease(ctx, rec.Engine())
		if err != nil {
			return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to load lease")
		}
		if (leftLease == roachpb.Lease{}) {
			log.Fatalf(ctx, "LHS of split has no lease")
		}

		replica, found := split.RightDesc.GetReplicaDescriptor(leftLease.Replica.StoreID)
		if !found {
			return enginepb.MVCCStats{}, result.Result{}, errors.Errorf(
				"pre-split lease holder %+v not found in post-split descriptor %+v",
				leftLease.Replica, split.RightDesc,
			)
		}
		rightLease := leftLease
		rightLease.Replica = replica

		gcThreshold, err := MakeStateLoader(rec).LoadGCThreshold(ctx, rec.Engine())
		if err != nil {
			return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to load GCThreshold")
		}
		if (*gcThreshold == hlc.Timestamp{}) {
			log.VEventf(ctx, 1, "LHS's GCThreshold of split is not set")
		}

		txnSpanGCThreshold, err := MakeStateLoader(rec).LoadTxnSpanGCThreshold(ctx, rec.Engine())
		if err != nil {
			return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to load TxnSpanGCThreshold")
		}
		if (*txnSpanGCThreshold == hlc.Timestamp{}) {
			log.VEventf(ctx, 1, "LHS's TxnSpanGCThreshold of split is not set")
		}

		// Writing the initial state is subtle since this also seeds the Raft
		// group. It becomes more subtle due to proposer-evaluated Raft.
		//
		// We are writing to the right hand side's Raft group state in this
		// batch so we need to synchronize with anything else that could be
		// touching that replica's Raft state. Specifically, we want to prohibit
		// an uninitialized Replica from receiving a message for the right hand
		// side range and performing raft processing. This is achieved by
		// serializing execution of uninitialized Replicas in Store.processRaft
		// and ensuring that no uninitialized Replica is being processed while
		// an initialized one (like the one currently being split) is being
		// processed.
		//
		// Since the right hand side of the split's Raft group may already
		// exist, we must be prepared to absorb an existing HardState. The Raft
		// group may already exist because other nodes could already have
		// processed the split and started talking to our node, prompting the
		// creation of a Raft group that can vote and bump its term, but not
		// much else: it can't receive snapshots because those intersect the
		// pre-split range; it can't apply log commands because it needs a
		// snapshot first.
		//
		// However, we can't absorb the right-hand side's HardState here because
		// we only *evaluate* the proposal here, but by the time it is
		// *applied*, the HardState could have changed. We do this downstream of
		// Raft, in splitPostApply, where we write the last index and the
		// HardState via a call to synthesizeRaftState. Here, we only call
		// writeInitialReplicaState which essentially writes a ReplicaState
		// only.
		rightMS, err = stateloader.WriteInitialReplicaState(
			ctx, rec.ClusterSettings(), batch, rightMS, split.RightDesc,
			rightLease, *gcThreshold, *txnSpanGCThreshold,
		)
		if err != nil {
			return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to write initial Replica state")
		}

		if !rec.ClusterSettings().Version.IsActive(cluster.VersionSplitHardStateBelowRaft) {
			// Write an initial state upstream of Raft even though it might
			// clobber downstream simply because that's what 1.0 does and if we
			// don't write it here, then a 1.0 version applying it as a follower
			// won't write a HardState at all and is guaranteed to crash.
			rsl := stateloader.Make(rec.ClusterSettings(), split.RightDesc.RangeID)
			if err := rsl.SynthesizeRaftState(ctx, batch); err != nil {
				return enginepb.MVCCStats{}, result.Result{}, errors.Wrap(err, "unable to synthesize initial Raft state")
			}
		}

		bothDeltaMS.Subtract(preRightMS)
		bothDeltaMS.Add(rightMS)
	}

	// Compute how much data the left-hand side has shed by splitting.
	// We've already recomputed that in absolute terms, so all we need to do is
	// to turn it into a delta so the upstream machinery can digest it.
	leftDeltaMS := leftMS // start with new left-hand side absolute stats
	recStats := rec.GetMVCCStats()
	leftDeltaMS.Subtract(recStats)        // subtract pre-split absolute stats
	leftDeltaMS.ContainsEstimates = false // if there were any, recomputation removed them

	// Perform a similar computation for the right hand side. The difference
	// is that there isn't yet a Replica which could apply these stats, so
	// they will go into the trigger to make the Store (which keeps running
	// counters) aware.
	rightDeltaMS := bothDeltaMS
	rightDeltaMS.Subtract(leftDeltaMS)
	var pd result.Result
	// This makes sure that no reads are happening in parallel; see #3148.
	pd.Replicated.BlockReads = true
	pd.Replicated.Split = &storagebase.Split{
		SplitTrigger: *split,
		RHSDelta:     rightDeltaMS,
	}
	return leftDeltaMS, pd, nil
}

// mergeTrigger is called on a successful commit of an AdminMerge transaction.
// It writes data from the right-hand range into the left-hand range and
// recomputes stats for the left-hand range.
func mergeTrigger(
	ctx context.Context,
	rec EvalContext,
	batch engine.Batch,
	ms *enginepb.MVCCStats,
	merge *roachpb.MergeTrigger,
	ts hlc.Timestamp,
) (result.Result, error) {
	desc := rec.Desc()
	if !bytes.Equal(desc.StartKey, merge.LeftDesc.StartKey) {
		return result.Result{}, errors.Errorf("LHS range start keys do not match: %s != %s",
			desc.StartKey, merge.LeftDesc.StartKey)
	}

	if !desc.EndKey.Less(merge.LeftDesc.EndKey) {
		return result.Result{}, errors.Errorf("original LHS end key is not less than the post merge end key: %s >= %s",
			desc.EndKey, merge.LeftDesc.EndKey)
	}

	if err := abortspan.New(merge.RightDesc.RangeID).CopyTo(
		ctx, batch, batch, ms, ts, merge.LeftDesc.RangeID,
	); err != nil {
		return result.Result{}, err
	}

	// The stats for the merged range are the sum of the LHS and RHS stats, less
	// the RHS's replicated range ID stats. The only replicated range ID keys we
	// copy from the RHS are the keys in the abort span, and we've already
	// accounted for those stats above.
	ms.Add(merge.RightMVCCStats)
	{
		ridPrefix := keys.MakeRangeIDReplicatedPrefix(merge.RightDesc.RangeID)
		iter := batch.NewIterator(engine.IterOptions{UpperBound: ridPrefix.PrefixEnd()})
		defer iter.Close()
		sysMS, err := iter.ComputeStats(
			engine.MakeMVCCMetadataKey(ridPrefix),
			engine.MakeMVCCMetadataKey(ridPrefix.PrefixEnd()),
			0 /* nowNanos */)
		if err != nil {
			return result.Result{}, err
		}
		ms.Subtract(sysMS)
	}

	var pd result.Result
	pd.Replicated.BlockReads = true
	pd.Replicated.Merge = &storagebase.Merge{
		MergeTrigger: *merge,
	}
	return pd, nil
}

func changeReplicasTrigger(
	ctx context.Context, rec EvalContext, batch engine.Batch, change *roachpb.ChangeReplicasTrigger,
) result.Result {
	var pd result.Result
	// After a successful replica addition or removal check to see if the
	// range needs to be split. Splitting usually takes precedence over
	// replication via configuration of the split and replicate queues, but
	// if the split occurs concurrently with the replicas change the split
	// can fail and won't retry until the next scanner cycle. Re-queuing
	// the replica here removes that latency.
	pd.Local.MaybeAddToSplitQueue = true

	// Gossip the first range whenever the range descriptor changes. We also
	// gossip the first range whenever the lease holder changes, but that might
	// not have occurred if a replica was being added or the non-lease-holder
	// replica was being removed. Note that we attempt the gossiping even from
	// the removed replica in case it was the lease-holder and it is still
	// holding the lease.
	pd.Local.GossipFirstRange = rec.IsFirstRange()

	var cpy roachpb.RangeDescriptor
	{
		desc := rec.Desc()
		cpy = *desc
	}
	cpy.Replicas = change.UpdatedReplicas
	cpy.NextReplicaID = change.NextReplicaID
	// TODO(tschottdorf): duplication of Desc with the trigger below, should
	// likely remove it from the trigger.
	pd.Replicated.State = &storagebase.ReplicaState{
		Desc: &cpy,
	}
	pd.Replicated.ChangeReplicas = &storagebase.ChangeReplicas{
		ChangeReplicasTrigger: *change,
	}

	return pd
}
