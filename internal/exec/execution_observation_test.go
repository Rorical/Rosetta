package exec

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func observationLogs(i ReactiveIntent) []rpc.Log {
	topics := []domain.Hash{keccak([]byte("Executed(address,address,uint256,uint256,uint256)")), {}, {}}
	copy(topics[1][12:], i.Call.Sender[:])
	copy(topics[2][12:], i.Route.Asset[:])
	data := make([]byte, 96)
	i.Route.Amount.FillBytes(data[:32])
	i.QuotedGross.FillBytes(data[64:])
	return []rpc.Log{{Address: i.Call.Executor, Topics: topics, Data: data}}
}

func TestObservationMatchesPreviousVariantAndPreservesNonce(t *testing.T) {
	i, _, _ := dispatchFixture(t)
	original := PendingTransaction{Hash: domain.Hash{1}, Intent: i, Nonce: 7}
	replacement := original
	replacement.Hash = domain.Hash{2}
	replacement.Previous = []PendingTransaction{original}
	d := &Dispatcher{}
	d.publish(DispatchState{Pending: &replacement})
	d.submissions.Store(original.Hash, SubmissionTiming{Hash: original.Hash, StartedAtNS: 100})
	position := ExecutionPosition{Stream: "flashblocks", PayloadID: "payload", BlockNumber: 12, FrameIndex: 3, TransactionIndex: 8, ReceivedAtNS: 200}
	if got := d.ObserveExecution(domain.Hash{3}, true, nil, position); got != nil {
		t.Fatal("unrelated transaction matched")
	}
	got := d.ObserveExecution(original.Hash, true, observationLogs(i), position)
	if got == nil || got.Error != "" || got.Profit.Cmp(i.QuotedGross) != 0 || got.QuoteError.Sign() != 0 || *got.SendToObservedNS != 100 {
		t.Fatal(got)
	}
	if !d.Included() || d.Pending().Hash != replacement.Hash {
		t.Fatal("preconfirmation released or changed nonce")
	}
	// Neither a revert nor an executed self-cancellation needs an executor event.
	got = d.ObserveExecution(original.Hash, false, nil, position)
	if got.Error != "" || got.Profit != nil || got.Success {
		t.Fatal(got)
	}
	replacement.Cancellation = &Cancellation{}
	d.publish(DispatchState{Pending: &replacement})
	got = d.ObserveExecution(replacement.Hash, true, nil, position)
	if !got.Cancelled || got.Error != "" || got.Profit != nil {
		t.Fatal(got)
	}
}

func TestObservationRejectsWrongEventAndRecordsAmbiguousSendTiming(t *testing.T) {
	i, limits, signer := dispatchFixture(t)
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "dispatch.json"), limits, signer, &dispatchNetworkFixture{fail: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err == nil {
		t.Fatal("expected uncertain response")
	}
	timing := d.Submission(hash)
	if timing == nil || timing.Acknowledged || timing.StartedAtNS == 0 || timing.ReturnedAtNS < timing.StartedAtNS {
		t.Fatal(timing)
	}
	logs := observationLogs(i)
	big.NewInt(1).FillBytes(logs[0].Data[:32])
	got := d.ObserveExecution(hash, true, logs, ExecutionPosition{ReceivedAtNS: time.Now().UnixNano()})
	if got.Error == "" || got.Profit != nil || !d.Included() {
		t.Fatal(got)
	}
}
