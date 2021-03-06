package power

import (
	"reflect"
	"sort"

	addr "github.com/filecoin-project/go-address"
	cid "github.com/ipfs/go-cid"
	errors "github.com/pkg/errors"
	xerrors "golang.org/x/xerrors"

	abi "github.com/filecoin-project/specs-actors/actors/abi"
	big "github.com/filecoin-project/specs-actors/actors/abi/big"
	. "github.com/filecoin-project/specs-actors/actors/util"
	adt "github.com/filecoin-project/specs-actors/actors/util/adt"
)

type State struct {
	TotalNetworkPower abi.StoragePower
	MinerCount        int64

	// The balances of pledge collateral for each miner actually held by this actor.
	// The sum of the values here should always equal the actor's balance.
	// See Claim for the pledge *requirements* for each actor.
	EscrowTable cid.Cid // BalanceTable (HAMT[address]TokenAmount)

	// A queue of events to be triggered by cron, indexed by epoch.
	CronEventQueue cid.Cid // Multimap, (HAMT[ChainEpoch]AMT[CronEvent]

	// Last chain epoch OnEpochTickEnd was called on
	LastEpochTick abi.ChainEpoch

	// Miners having failed to prove storage.
	PoStDetectedFaultMiners cid.Cid // Set, HAMT[addr.Address]struct{}

	// Claimed power and associated pledge requirements for each miner.
	Claims cid.Cid // Map, HAMT[address]Claim

	// Number of miners having proven the minimum consensus power.
	// TODO: actually update this value.
	NumMinersMeetingMinPower int64
}

type Claim struct {
	// Sum of power for a miner's sectors.
	Power abi.StoragePower
	// Sum of pledge requirement for a miner's sectors.
	Pledge abi.TokenAmount
}

type CronEvent struct {
	MinerAddr       addr.Address
	CallbackPayload []byte
}

type AddrKey = adt.AddrKey

func ConstructState(emptyMapCid cid.Cid) *State {
	return &State{
		TotalNetworkPower:        abi.NewStoragePower(0),
		EscrowTable:              emptyMapCid,
		CronEventQueue:           emptyMapCid,
		PoStDetectedFaultMiners:  emptyMapCid,
		Claims:                   emptyMapCid,
		NumMinersMeetingMinPower: 0,
	}
}

// Note: this method is currently (Feb 2020) unreferenced in the actor code, but expected to be used to validate
// Election PoSt winners outside the chain state. We may remove it.
func (st *State) minerNominalPowerMeetsConsensusMinimum(s adt.Store, miner addr.Address) (bool, error) {
	claim, ok, err := st.getClaim(s, miner)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, errors.Errorf("no claim for actor %v", miner)
	}

	minerNominalPower, err := st.computeNominalPower(s, miner, claim.Power)
	if err != nil {
		return false, err
	}

	// if miner is larger than min power requirement, we're set
	if minerNominalPower.GreaterThanEqual(ConsensusMinerMinPower) {
		return true, nil
	}

	// otherwise, if another miner meets min power requirement, return false
	if st.NumMinersMeetingMinPower > 0 {
		return false, nil
	}

	// else if none do, check whether in MIN_MINER_SIZE_TARG miners
	if st.MinerCount <= ConsensusMinerMinMiners {
		// miner should pass
		return true, nil
	}

	var minerSizes []abi.StoragePower
	var claimed Claim
	if err := adt.AsMap(s, st.Claims).ForEach(&claimed, func(k string) error {
		maddr, err := addr.NewFromBytes([]byte(k))
		if err != nil {
			return err
		}
		nominalPower, err := st.computeNominalPower(s, maddr, claimed.Power)
		if err != nil {
			return err
		}
		minerSizes = append(minerSizes, nominalPower)
		return nil
	}); err != nil {
		return false, errors.Wrap(err, "failed to iterate power table")
	}

	// get size of MIN_MINER_SIZE_TARGth largest miner
	sort.Slice(minerSizes, func(i, j int) bool { return i > j })
	return minerNominalPower.GreaterThanEqual(minerSizes[ConsensusMinerMinMiners-1]), nil
}

func (st *State) getMinerBalance(store adt.Store, miner addr.Address) (abi.TokenAmount, error) {
	table := adt.AsBalanceTable(store, st.EscrowTable)
	return table.Get(miner)
}

func (st *State) setMinerBalance(store adt.Store, miner addr.Address, amount abi.TokenAmount) error {
	table := adt.AsBalanceTable(store, st.EscrowTable)
	if table.Set(miner, amount) == nil {
		st.EscrowTable = table.Root()
	}
	return nil
}

func (st *State) addMinerBalance(store adt.Store, miner addr.Address, amount abi.TokenAmount) error {
	Assert(amount.GreaterThanEqual(big.Zero()))
	table := adt.AsBalanceTable(store, st.EscrowTable)
	if table.Add(miner, amount) == nil {
		st.EscrowTable = table.Root()
	}
	return table.Add(miner, amount)
}

func (st *State) subtractMinerBalance(store adt.Store, miner addr.Address, amount abi.TokenAmount,
	balanceFloor abi.TokenAmount) (abi.TokenAmount, error) {
	Assert(amount.GreaterThanEqual(big.Zero()))
	table := adt.AsBalanceTable(store, st.EscrowTable)
	subtracted, err := table.SubtractWithMinimum(miner, amount, balanceFloor)
	if err == nil {
		st.EscrowTable = table.Root()
	}
	return subtracted, err
}

// Parameters may be negative to subtract.
func (st *State) AddToClaim(s adt.Store, miner addr.Address, power abi.StoragePower, pledge abi.TokenAmount) error {
	claim, ok, err := st.getClaim(s, miner)
	if err != nil {
		return err
	}
	if !ok {
		return errors.Errorf("no claim for actor %v", miner)
	}

	oldNominalPower, err := st.computeNominalPower(s, miner, claim.Power)
	if err != nil {
		return err
	}

	// update pledge and power
	// TODO: ZX, update to ensure that pledge is appropriate for given power update
	claim.Power = big.Add(claim.Power, power)
	claim.Pledge = big.Add(claim.Pledge, pledge)

	newNominalPower, err := st.computeNominalPower(s, miner, claim.Power)
	if err != nil {
		return err
	}

	prevBelow := oldNominalPower.LessThan(ConsensusMinerMinPower)
	stillBelow := newNominalPower.LessThan(ConsensusMinerMinPower)

	faulty, err := st.hasDetectedFault(s, miner)
	if err != nil {
		return xerrors.Errorf("Failed to check if miner was faulty: %w", err)
	}

	if !faulty {
		if prevBelow && !stillBelow {
			// just passed min miner size
			st.NumMinersMeetingMinPower++
			st.TotalNetworkPower = big.Add(st.TotalNetworkPower, newNominalPower)
		} else if !prevBelow && stillBelow {
			// just went below min miner size
			st.NumMinersMeetingMinPower--
			st.TotalNetworkPower = big.Sub(st.TotalNetworkPower, oldNominalPower)
		} else if !prevBelow && !stillBelow {
			// Was above the threshold, still above
			st.TotalNetworkPower = big.Add(st.TotalNetworkPower, power)
		}
	}

	claim.Pledge = big.Add(claim.Pledge, pledge)

	AssertMsg(claim.Power.GreaterThanEqual(big.Zero()), "negative claimed power: %v", claim.Power)
	AssertMsg(claim.Pledge.GreaterThanEqual(big.Zero()), "negative claimed pledge: %v", claim.Pledge)
	AssertMsg(st.NumMinersMeetingMinPower >= 0, "negative number of miners larger than min: %v", st.NumMinersMeetingMinPower)
	return st.setClaim(s, miner, claim)
}

func (st *State) computeNominalPower(s adt.Store, minerAddr addr.Address, claimedPower abi.StoragePower) (abi.StoragePower, error) {
	// Compute nominal power: i.e., the power we infer the miner to have (based on the network's
	// PoSt queries), which may not be the same as the claimed power.
	// Currently, the nominal power may differ from claimed power because of
	// detected faults.
	nominalPower := claimedPower
	if found, err := st.hasDetectedFault(s, minerAddr); err != nil {
		return abi.NewStoragePower(0), err
	} else if found {
		nominalPower = big.Zero()
	}
	// no need to account for declared faults, since they
	// are already accounted for in "claimed" power
	// Likewise miners will never be undercollateralized so no
	// check needed here

	return nominalPower, nil
}

func (st *State) hasDetectedFault(s adt.Store, a addr.Address) (bool, error) {
	faultyMiners := adt.AsSet(s, st.PoStDetectedFaultMiners)
	found, err := faultyMiners.Has(AddrKey(a))
	if err != nil {
		return false, errors.Wrapf(err, "failed to get detected faults for address %v from set %s", a, st.PoStDetectedFaultMiners)
	}
	return found, nil
}

func (st *State) putDetectedFault(s adt.Store, a addr.Address) error {

	// prior to making change verify whether we've lose a miner > min size
	claim, ok, err := st.getClaim(s, a)
	if err != nil {
		return err
	}
	if !ok {
		return errors.Errorf("no claim for actor %v", a)
	}
	// could be made more efficient by just taking claimed power given
	// how computeNominal currently works, but that could change.
	nominalPower, err := st.computeNominalPower(s, a, claim.Power)
	if err != nil {
		return err
	}
	if nominalPower.GreaterThanEqual(ConsensusMinerMinPower) {
		// just lost a miner > min size
		st.NumMinersMeetingMinPower--
	}

	faultyMiners := adt.AsSet(s, st.PoStDetectedFaultMiners)
	if err := faultyMiners.Put(AddrKey(a)); err != nil {
		return errors.Wrapf(err, "failed to put detected fault for miner %s in set %s", a, st.PoStDetectedFaultMiners)
	}
	st.PoStDetectedFaultMiners = faultyMiners.Root()

	return nil
}

func (st *State) deleteDetectedFault(s adt.Store, a addr.Address) error {
	faultyMiners := adt.AsSet(s, st.PoStDetectedFaultMiners)
	if err := faultyMiners.Delete(AddrKey(a)); err != nil {
		return errors.Wrapf(err, "failed to delete storage power at address %s from set %s", a, st.PoStDetectedFaultMiners)
	}
	st.PoStDetectedFaultMiners = faultyMiners.Root()

	claim, ok, err := st.getClaim(s, a)
	if err != nil {
		return err
	}
	if !ok {
		return errors.Errorf("no claim for actor %v", a)
	}
	nominalPower, err := st.computeNominalPower(s, a, claim.Power)
	if err != nil {
		return err
	}
	if nominalPower.GreaterThanEqual(ConsensusMinerMinPower) {
		// just regained a miner > min size
		st.NumMinersMeetingMinPower++
		st.TotalNetworkPower = big.Add(st.TotalNetworkPower, claim.Power)
	}

	return nil
}

func (st *State) appendCronEvent(store adt.Store, epoch abi.ChainEpoch, event *CronEvent) error {
	mmap := adt.AsMultimap(store, st.CronEventQueue)
	err := mmap.Add(epochKey(epoch), event)
	if err != nil {
		return errors.Wrapf(err, "failed to store cron event at epoch %v for miner %v", epoch, event)
	}
	st.CronEventQueue = mmap.Root()
	return nil
}

func (st *State) loadCronEvents(store adt.Store, epoch abi.ChainEpoch) ([]CronEvent, error) {
	mmap := adt.AsMultimap(store, st.CronEventQueue)
	var events []CronEvent
	var ev CronEvent
	err := mmap.ForEach(epochKey(epoch), &ev, func(i int64) error {
		// Ignore events for defunct miners.
		if _, found, err := st.getClaim(store, ev.MinerAddr); err != nil {
			return errors.Wrapf(err, "failed to find claimed power for %v for cron event", ev.MinerAddr)
		} else if found {
			events = append(events, ev)
		}
		return nil
	})
	return events, err
}

func (st *State) clearCronEvents(store adt.Store, epoch abi.ChainEpoch) error {
	mmap := adt.AsMultimap(store, st.CronEventQueue)
	err := mmap.RemoveAll(epochKey(epoch))
	if err != nil {
		return errors.Wrapf(err, "failed to clear cron events")
	}
	st.CronEventQueue = mmap.Root()
	return nil
}

func (st *State) getClaim(s adt.Store, a addr.Address) (*Claim, bool, error) {
	hm := adt.AsMap(s, st.Claims)

	var out Claim
	found, err := hm.Get(AddrKey(a), &out)
	if err != nil {
		return nil, false, errors.Wrapf(err, "failed to get claim for address %v from store %s", a, st.Claims)
	}
	if !found {
		return nil, false, nil
	}
	return &out, true, nil
}

func (st *State) setClaim(s adt.Store, a addr.Address, claim *Claim) error {
	Assert(claim.Power.GreaterThanEqual(big.Zero()))
	Assert(claim.Pledge.GreaterThanEqual(big.Zero()))

	hm := adt.AsMap(s, st.Claims)

	if err := hm.Put(AddrKey(a), claim); err != nil {
		return errors.Wrapf(err, "failed to put claim with address %s power %v in store %s", a, claim, st.Claims)
	}
	st.Claims = hm.Root()
	return nil
}

func (st *State) deleteClaim(s adt.Store, a addr.Address) error {
	hm := adt.AsMap(s, st.Claims)

	if err := hm.Delete(AddrKey(a)); err != nil {
		return errors.Wrapf(err, "failed to delete claim at address %s from store %s", a, st.Claims)
	}
	st.Claims = hm.Root()
	return nil
}

func addrInArray(a addr.Address, list []addr.Address) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func epochKey(e abi.ChainEpoch) adt.Keyer {
	return adt.IntKey(int64(e))
}

func init() {
	// Check that ChainEpoch is indeed a signed integer to confirm that epochKey is making the right interpretation.
	var e abi.ChainEpoch
	if reflect.TypeOf(e).Kind() != reflect.Int64 {
		panic("incorrect chain epoch encoding")
	}
}
