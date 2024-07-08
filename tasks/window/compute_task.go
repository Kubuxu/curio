package window

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/samber/lo"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/harmony/resources"
	"github.com/filecoin-project/curio/lib/chainsched"
	"github.com/filecoin-project/curio/lib/paths"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/actors/policy"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/promise"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/filecoin-project/lotus/storage/sealer"
	"github.com/filecoin-project/lotus/storage/sealer/sealtasks"
	"github.com/filecoin-project/lotus/storage/sealer/storiface"
)

var log = logging.Logger("curio/window")

var EpochsPerDeadline = miner.WPoStProvingPeriod() / abi.ChainEpoch(miner.WPoStPeriodDeadlines)

type WdPostTaskDetails struct {
	Ts       *types.TipSet
	Deadline *dline.Info
}

type WDPoStAPI interface {
	ChainHead(context.Context) (*types.TipSet, error)
	ChainGetTipSet(context.Context, types.TipSetKey) (*types.TipSet, error)
	StateMinerProvingDeadline(context.Context, address.Address, types.TipSetKey) (*dline.Info, error)
	StateMinerInfo(context.Context, address.Address, types.TipSetKey) (api.MinerInfo, error)
	ChainGetTipSetAfterHeight(context.Context, abi.ChainEpoch, types.TipSetKey) (*types.TipSet, error)
	StateMinerPartitions(context.Context, address.Address, uint64, types.TipSetKey) ([]api.Partition, error)
	StateGetRandomnessFromBeacon(ctx context.Context, personalization crypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte, tsk types.TipSetKey) (abi.Randomness, error)
	StateNetworkVersion(context.Context, types.TipSetKey) (network.Version, error)
	StateMinerSectors(context.Context, address.Address, *bitfield.BitField, types.TipSetKey) ([]*miner.SectorOnChainInfo, error)
}

type ProverPoSt interface {
	GenerateWindowPoStAdv(ctx context.Context, ppt abi.RegisteredPoStProof, mid abi.ActorID, sectors []storiface.PostSectorChallenge, partitionIdx int, randomness abi.PoStRandomness, allowSkip bool) (storiface.WindowPoStResult, error)
}

type WdPostTask struct {
	api WDPoStAPI
	db  *harmonydb.DB

	faultTracker sealer.FaultTracker
	storage      paths.Store
	verifier     storiface.Verifier
	paramsReady  func() (bool, error)

	windowPoStTF promise.Promise[harmonytask.AddTaskFunc]

	actors               map[dtypes.MinerAddress]bool
	max                  int
	parallel             chan struct{}
	challengeReadTimeout time.Duration
}

type wdTaskIdentity struct {
	SpID               uint64         `db:"sp_id"`
	ProvingPeriodStart abi.ChainEpoch `db:"proving_period_start"`
	DeadlineIndex      uint64         `db:"deadline_index"`
	PartitionIndex     uint64         `db:"partition_index"`
}

func NewWdPostTask(db *harmonydb.DB,
	api WDPoStAPI,
	faultTracker sealer.FaultTracker,
	storage paths.Store,
	verifier storiface.Verifier,
	paramck func() (bool, error),
	pcs *chainsched.CurioChainSched,
	actors map[dtypes.MinerAddress]bool,
	max int,
	parallel int,
	challengeReadTimeout time.Duration,
) (*WdPostTask, error) {
	t := &WdPostTask{
		db:  db,
		api: api,

		faultTracker: faultTracker,
		storage:      storage,
		verifier:     verifier,
		paramsReady:  paramck,

		actors:               actors,
		max:                  max,
		challengeReadTimeout: challengeReadTimeout,
	}
	if parallel > 0 {
		t.parallel = make(chan struct{}, parallel)
	}

	if pcs != nil {
		if err := pcs.AddHandler(t.processHeadChange); err != nil {
			return nil, err
		}
	}

	return t, nil
}

func (t *WdPostTask) Do(taskID harmonytask.TaskID, stillOwned func() bool) (done bool, err error) {
	log.Debugw("WdPostTask.Do()", "taskID", taskID)

	var spID, pps, dlIdx, partIdx uint64

	err = t.db.QueryRow(context.Background(),
		`Select sp_id, proving_period_start, deadline_index, partition_index
			from wdpost_partition_tasks 
			where task_id = $1`, taskID).Scan(
		&spID, &pps, &dlIdx, &partIdx,
	)
	if err != nil {
		log.Errorf("WdPostTask.Do() failed to queryRow: %v", err)
		return false, err
	}

	head, err := t.api.ChainHead(context.Background())
	if err != nil {
		log.Errorf("WdPostTask.Do() failed to get chain head: %v", err)
		return false, err
	}

	deadline := NewDeadlineInfo(abi.ChainEpoch(pps), dlIdx, head.Height())

	var testTask *int
	isTestTask := func() bool {
		if testTask != nil {
			return *testTask > 0
		}

		testTask = new(int)
		err := t.db.QueryRow(context.Background(), `SELECT COUNT(*) FROM harmony_test WHERE task_id = $1`, taskID).Scan(testTask)
		if err != nil {
			log.Errorf("WdPostTask.Do() failed to queryRow: %v", err)
			return false
		}

		return *testTask > 0
	}

	if deadline.PeriodElapsed() && !isTestTask() {
		log.Errorf("WdPost removed stale task: %v %v", taskID, deadline)
		return true, nil
	}

	if deadline.Challenge > head.Height() {
		if isTestTask() {
			deadline = NewDeadlineInfo(abi.ChainEpoch(pps)-deadline.WPoStProvingPeriod, dlIdx, head.Height()-deadline.WPoStProvingPeriod)
			log.Warnw("Test task is in the future, adjusting to past", "taskID", taskID, "deadline", deadline)
		}
	}

	maddr, err := address.NewIDAddress(spID)
	if err != nil {
		log.Errorf("WdPostTask.Do() failed to NewIDAddress: %v", err)
		return false, err
	}

	ts, err := t.api.ChainGetTipSetAfterHeight(context.Background(), deadline.Challenge, head.Key())
	if err != nil {
		log.Errorf("WdPostTask.Do() failed to ChainGetTipSetAfterHeight: %v", err)
		return false, err
	}

	postOut, err := t.DoPartition(context.Background(), ts, maddr, deadline, partIdx)
	if err != nil {
		log.Errorf("WdPostTask.Do() failed to doPartition: %v", err)
		return false, err
	}

	var msgbuf bytes.Buffer
	if err := postOut.MarshalCBOR(&msgbuf); err != nil {
		return false, xerrors.Errorf("marshaling PoSt: %w", err)
	}

	if isTestTask() {
		// Do not send test tasks to the chain but to harmony_test & stdout.

		data, err := json.MarshalIndent(map[string]any{
			"sp_id":                spID,
			"proving_period_start": pps,
			"deadline":             deadline.Index,
			"partition":            partIdx,
			"submit_at_epoch":      deadline.Open,
			"submit_by_epoch":      deadline.Close,
			"post_out":             postOut,
			"proof_params":         msgbuf.Bytes(),
		}, "", "  ")
		if err != nil {
			return false, xerrors.Errorf("marshaling message: %w", err)
		}
		ctx := context.Background()
		_, err = t.db.Exec(ctx, `UPDATE harmony_test SET result=$1 WHERE task_id=$2`, string(data), taskID)
		if err != nil {
			return false, xerrors.Errorf("updating harmony_test: %w", err)
		}
		log.Infof("SKIPPED sending test message to chain. SELECT * FROM harmony_test WHERE task_id= %v", taskID)
		return true, nil // nothing committed
	}
	// Insert into wdpost_proofs table
	n, err := t.db.Exec(context.Background(),
		`INSERT INTO wdpost_proofs (
                               sp_id,
                               proving_period_start,
	                           deadline,
	                           partition,
	                           submit_at_epoch,
	                           submit_by_epoch,
                               proof_params)
	    			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		spID,
		pps,
		deadline.Index,
		partIdx,
		deadline.Open,
		deadline.Close,
		msgbuf.Bytes(),
	)

	if err != nil {
		log.Errorf("WdPostTask.Do() failed to insert into wdpost_proofs: %v", err)
		return false, err
	}
	if n != 1 {
		log.Errorf("WdPostTask.Do() failed to insert into wdpost_proofs: %v", err)
		return false, err
	}

	return true, nil
}

func entToStr[T any](t T, i int) string {
	return fmt.Sprint(t)
}

func (t *WdPostTask) CanAccept(ids []harmonytask.TaskID, si *harmonytask.SchedulingInfo) ([]harmonytask.TaskAndBid, error) {
	rdy, err := t.paramsReady()
	if err != nil {
		return nil, xerrors.Errorf("failed to setup params: %w", err)
	}
	if !rdy {
		log.Infow("WdPostTask.CanAccept() params not ready, not scheduling")
		return nil, nil
	}

	// GetEpoch
	ts, err := t.api.ChainHead(context.Background())

	if err != nil {
		return nil, err
	}

	// GetData for tasks
	type wdTaskDef struct {
		TaskID             harmonytask.TaskID
		SpID               uint64
		ProvingPeriodStart abi.ChainEpoch
		DeadlineIndex      uint64
		PartitionIndex     uint64

		dlInfo *dline.Info `pgx:"-"`
	}
	var tasks []wdTaskDef

	err = t.db.Select(context.Background(), &tasks,
		`Select 
			task_id,
			sp_id,
			proving_period_start,
			deadline_index,
			partition_index
	from wdpost_partition_tasks 
	where task_id IN (SELECT unnest(string_to_array($1, ','))::bigint)`, strings.Join(lo.Map(ids, entToStr[harmonytask.TaskID]), ","))
	if err != nil {
		return nil, err
	}

	// Accept those past deadline, then delete them in Do().
	f := lo.Filter(tasks, func(_ wdTaskDef, i int) bool {
		tasks[i].dlInfo = NewDeadlineInfo(tasks[i].ProvingPeriodStart, tasks[i].DeadlineIndex, ts.Height())
		return tasks[i].dlInfo.PeriodElapsed()
	})
	if len(f) > 0 {
		log.Infof("WdPostTask.CanAccept() found %d tasks past deadline", len(f))
		return lo.Map(f, func(d wdTaskDef, _ int) harmonytask.TaskAndBid {
			return harmonytask.TaskAndBid{TaskID: d.TaskID, Bid: 1000}
		}), nil
	}

	//////////////////Bidding Logic Starts Here//////////////////

	// Select the one closest to the deadline
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].dlInfo.Open < tasks[j].dlInfo.Open
	})

	return lo.Map(tasks, func(d wdTaskDef, idx int) harmonytask.TaskAndBid {
		bid := len(tasks) + 10 - idx
		{
			var r int
			err := t.db.QueryRow(context.Background(), `SELECT COUNT(*) 
		FROM harmony_task_history 
		WHERE task_id = $1 AND result = false`, d.TaskID).Scan(&r)
			if err != nil {
				log.Errorf("WdPostTask.CanAccept() failed to queryRow: %v", err)
			}
			bid -= r
		}
		return harmonytask.TaskAndBid{
			TaskID: d.TaskID,
			Bid:    bid,
		}
	}), nil
}

var res = storiface.ResourceTable[sealtasks.TTGenerateWindowPoSt]

func (t *WdPostTask) TypeDetails() harmonytask.TaskTypeDetails {
	return harmonytask.TaskTypeDetails{
		Name:        "WdPost",
		Max:         t.max,
		MaxFailures: 5,
		Follows:     nil,
		Cost: resources.Resources{
			Cpu: 1,

			// todo set to something for 32/64G sector sizes? Technically windowPoSt is happy on a CPU
			//  but it will use a GPU if available
			Gpu: 0,

			// RAM of smallest proof's max is listed here
			Ram: wdPostRam{
				t.db,
				t.api,
				resources.Ram(lo.Reduce(lo.Keys(res), func(i uint64, k abi.RegisteredSealProof, _ int) uint64 {
					if res[k].MaxMemory < i {
						return res[k].MaxMemory
					}
					return i
				}, 1<<63))}, // Used for HasCapacity
		},
	}
}

type wdPostRam struct {
	Db            *harmonydb.DB
	Api           WDPoStAPI
	resources.Ram // Used for HasCapacity
}

func (r wdPostRam) Claim(taskID int) (func() error, error) {
	// how much ram does this task's proof need?
	var spID uint64
	err := r.Db.QueryRow(context.Background(),
		`Select sp_id
		from wdpost_partition_tasks 
		where task_id = $1`, taskID).Scan(&spID)
	if err != nil {
		log.Errorf("WdPostTask.Do() failed to queryRow: %v", err)
		return nil, err
	}
	maddr, err := address.NewIDAddress(spID)
	if err != nil {
		log.Errorf("WdPostTask.CanAccept() failed to NewIDAddress: %v", err)
		return nil, err
	}

	ts, err := r.Api.ChainHead(context.Background()) // TODO is this too heavy for how frequently this is called?
	if err != nil {
		log.Errorf("WdPostTask.CanAccept() failed to get chain head: %v", err)
		return nil, err
	}
	mi, err := r.Api.StateMinerInfo(context.Background(), maddr, ts.Key())
	if err != nil {
		log.Errorf("WdPostTask.CanAccept() failed to StateMinerInfo: %v", err)
		return nil, err
	}

	spt, err := policy.GetSealProofFromPoStProof(mi.WindowPoStProofType)
	if err != nil {
		log.Errorf("WdPostTask.CanAccept() failed to GetSealProofFromPoStProof: %v", err)
		return nil, err
	}

	return resources.Ram(res[spt].MaxMemory).Claim(taskID)
}

var _ resources.Dynamic = wdPostRam{}

func (t *WdPostTask) Adder(taskFunc harmonytask.AddTaskFunc) {
	t.windowPoStTF.Set(taskFunc)
}

func (t *WdPostTask) processHeadChange(ctx context.Context, revert, apply *types.TipSet) error {
	for act := range t.actors {
		maddr := address.Address(act)

		aid, err := address.IDFromAddress(maddr)
		if err != nil {
			return xerrors.Errorf("getting miner ID: %w", err)
		}

		di, err := t.api.StateMinerProvingDeadline(ctx, maddr, apply.Key())
		if err != nil {
			return err
		}

		if !di.PeriodStarted() {
			return nil // not proving anything yet
		}

		partitions, err := t.api.StateMinerPartitions(ctx, maddr, di.Index, apply.Key())
		if err != nil {
			return xerrors.Errorf("getting partitions: %w", err)
		}

		// TODO: Batch Partitions??

		for pidx := range partitions {
			tid := wdTaskIdentity{
				SpID:               aid,
				ProvingPeriodStart: di.PeriodStart,
				DeadlineIndex:      di.Index,
				PartitionIndex:     uint64(pidx),
			}

			tf := t.windowPoStTF.Val(ctx)
			if tf == nil {
				return xerrors.Errorf("no task func")
			}

			tf(func(id harmonytask.TaskID, tx *harmonydb.Tx) (bool, error) {
				return t.addTaskToDB(id, tid, tx)
			})
		}
	}

	return nil
}

func (t *WdPostTask) addTaskToDB(taskId harmonytask.TaskID, taskIdent wdTaskIdentity, tx *harmonydb.Tx) (bool, error) {

	_, err := tx.Exec(
		`INSERT INTO wdpost_partition_tasks (
                         task_id,
                          sp_id,
                          proving_period_start,
                          deadline_index,
                          partition_index
                        ) VALUES ($1, $2, $3, $4, $5)`,
		taskId,
		taskIdent.SpID,
		taskIdent.ProvingPeriodStart,
		taskIdent.DeadlineIndex,
		taskIdent.PartitionIndex,
	)
	if err != nil {
		return false, xerrors.Errorf("insert partition task: %w", err)
	}

	return true, nil
}

var _ harmonytask.BidTask = &WdPostTask{}

func NewDeadlineInfo(periodStart abi.ChainEpoch, deadlineIdx uint64, currEpoch abi.ChainEpoch) *dline.Info {
	return dline.NewInfo(periodStart, deadlineIdx, currEpoch, miner.WPoStPeriodDeadlines, miner.WPoStProvingPeriod(), miner.WPoStChallengeWindow(), miner.WPoStChallengeLookback, miner.FaultDeclarationCutoff)
}
