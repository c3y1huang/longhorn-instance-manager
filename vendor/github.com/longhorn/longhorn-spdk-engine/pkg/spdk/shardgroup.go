package spdk

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"

	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	commonbitmap "github.com/longhorn/go-common-libs/bitmap"
	commonnet "github.com/longhorn/go-common-libs/net"
	"github.com/longhorn/go-spdk-helper/pkg/jsonrpc"
	spdkclient "github.com/longhorn/go-spdk-helper/pkg/spdk/client"
	spdktypes "github.com/longhorn/go-spdk-helper/pkg/spdk/types"
	helpertypes "github.com/longhorn/go-spdk-helper/pkg/types"
	"github.com/longhorn/types/pkg/generated/spdkrpc"

	"github.com/longhorn/longhorn-spdk-engine/pkg/api"
	"github.com/longhorn/longhorn-spdk-engine/pkg/types"
	"github.com/longhorn/longhorn-spdk-engine/pkg/util"

	safelog "github.com/longhorn/longhorn-spdk-engine/pkg/log"
)

// NVMe-oF connection timeouts for the ShardGroup process's connections to
// shard endpoints. Tuned longer than the replica equivalents so that brief
// network blips do not flap an EC slot to FAILED before reconnect can land.
//
// See LEP "NVMe-oF transient failure vs permanent disk failure".
const (
	ecShardCtrlrLossTimeoutSec  = 120
	ecShardFastIOFailTimeoutSec = 15
)

// ShardGroup is the SPDK-side process state for an EC volume's lvstore + head
// lvol layer. Each ShardGroup process owns:
//
//   - k+m NVMe-oF connections to shard endpoints (the base bdevs).
//   - A bdev_ec on top of those shard bdevs.
//   - A per-volume lvstore on top of bdev_ec.
//   - A head lvol in the lvstore, exposed via NVMe-oF for the engine to consume.
//
// The lvstore and head lvol survive volume detach (cleanupRequired=false on
// Delete). They are torn down only on volume delete (cleanupRequired=true).
// This cleanupRequired discipline is what prevents the EC-volume detach
// data-loss bug — detach must never call BdevLvolDeleteLvstore on the
// bdev_ec-backed lvstore.
type ShardGroup struct {
	sync.RWMutex

	ctx context.Context

	Name       string // typically equals VolumeName
	VolumeName string

	// EC parameters, immutable after Create.
	DataChunks   uint32
	ParityChunks uint32
	StripSizeKb  uint32
	SpecSize     uint64

	// SalvageRequested selects the recovery path on Create: tolerate
	// missing shard connections (passing "" for unreachable slots) and
	// skip lvstore + head lvol creation — SPDK's bdev_examine
	// auto-imports the existing lvstore from the encoded shard blocks.
	// Set by the controller for ShardGroup process re-provisioning on a
	// new node (engine-node failover) or after process crash.
	SalvageRequested bool

	// Upstream shard endpoints keyed by Shard CR external name
	// (<volumeName>-<slotIndex>). Populated at Create time from
	// ShardGroupSpec.shards.
	Shards map[string]*ShardEndpoint

	// SPDK names for the layered bdev stack.
	EcBdevName   string // <volumeName>-ec
	LvsName      string // <volumeName>-lvs
	LvsUUID      string // populated after lvstore creation
	HeadLvolName string // == VolumeName
	HeadLvolUUID string // populated after head lvol creation
	HeadAlias    string // <LvsName>/<HeadLvolName>
	Nqn          string // NVMe-oF subsystem NQN for the exposed head lvol

	// Cached snapshot lineage for ShardGroupGet. Refreshed by
	// refreshECSnapshotMapNoLock() after create/expand/snapshot operations.
	Head        *api.Lvol
	SnapshotMap map[string]*api.Lvol
	ActualSize  uint64

	IP   string
	Port int32

	State    types.InstanceState
	ErrorMsg string

	IsExposed bool

	// UpdateCh should not be protected by the ShardGroup lock.
	UpdateCh chan interface{}

	log *safelog.SafeLogger
}

// ShardEndpoint is the upstream shard address and slot index that the
// ShardGroup process consumes as a base bdev for bdev_ec.
type ShardEndpoint struct {
	Address   string // ip:port
	SlotIndex uint32
}

// GetShardGroupEcBdevName returns the SPDK bdev name for the per-volume bdev_ec.
func GetShardGroupEcBdevName(volumeName string) string {
	return fmt.Sprintf("%s-ec", volumeName)
}

// GetShardGroupLvsName returns the SPDK lvstore name on the bdev_ec.
func GetShardGroupLvsName(volumeName string) string {
	return fmt.Sprintf("%s-lvs", volumeName)
}

// NewShardGroup constructs a ShardGroup in InstanceStatePending. A subsequent
// Create call materializes the SPDK stack on disk.
//
// salvageRequested=true selects the recovery path on Create — tolerate missing
// shard connections and skip lvstore + head lvol creation, letting bdev_examine
// re-discover the existing lvstore from the encoded blocks.
func NewShardGroup(ctx context.Context, name, volumeName string, specSize uint64,
	dataChunks, parityChunks, stripSizeKb uint32, shards map[string]*ShardEndpoint,
	salvageRequested bool, updateCh chan interface{}) *ShardGroup {

	log := logrus.StandardLogger().WithFields(logrus.Fields{
		"shardGroupName": name,
		"volumeName":     volumeName,
		"dataChunks":     dataChunks,
		"parityChunks":   parityChunks,
		"stripSizeKb":    stripSizeKb,
	})

	roundedSize := util.RoundUp(specSize, helpertypes.MiB)
	if roundedSize != specSize {
		log.Infof("Rounded up size from %v to %v since the size should be a multiple of MiB", specSize, roundedSize)
	}
	log = log.WithField("specSize", roundedSize)

	lvsName := GetShardGroupLvsName(volumeName)
	headLvolName := volumeName

	return &ShardGroup{
		ctx: ctx,

		Name:       name,
		VolumeName: volumeName,

		DataChunks:       dataChunks,
		ParityChunks:     parityChunks,
		StripSizeKb:      stripSizeKb,
		SpecSize:         roundedSize,
		SalvageRequested: salvageRequested,

		Shards: shards,

		EcBdevName:   GetShardGroupEcBdevName(volumeName),
		LvsName:      lvsName,
		HeadLvolName: headLvolName,
		HeadAlias:    spdktypes.GetLvolAlias(lvsName, headLvolName),
		Nqn:          helpertypes.GetNQN(headLvolName),

		SnapshotMap: map[string]*api.Lvol{},

		State: types.InstanceStatePending,

		UpdateCh: updateCh,

		log: safelog.NewSafeLogger(log),
	}
}

// ServiceShardGroupToProtoShardGroup converts in-memory ShardGroup state to the
// gRPC response message. EcStatus is left nil; it is populated by
// ShardGroup.Get via live SPDK queries in later commits.
func ServiceShardGroupToProtoShardGroup(sg *ShardGroup) *spdkrpc.ShardGroup {
	snapshots := make(map[string]*spdkrpc.Lvol, len(sg.SnapshotMap))
	for snapshotName, snapshotLvol := range sg.SnapshotMap {
		snapshots[snapshotName] = api.LvolToProtoLvol(snapshotLvol)
	}

	return &spdkrpc.ShardGroup{
		Name:             sg.Name,
		VolumeName:       sg.VolumeName,
		SpecSize:         sg.SpecSize,
		DataChunks:       sg.DataChunks,
		ParityChunks:     sg.ParityChunks,
		StripSizeKb:      sg.StripSizeKb,
		EcBdevName:       sg.EcBdevName,
		LvsName:          sg.LvsName,
		LvsUuid:          sg.LvsUUID,
		HeadLvolName:     sg.HeadLvolName,
		HeadLvolUuid:     sg.HeadLvolUUID,
		NvmfSubsystemNqn: sg.Nqn,
		Ip:               sg.IP,
		Port:             sg.Port,
		ProcessState:     instanceStateToProcessState(sg.State),
		ErrorMsg:         sg.ErrorMsg,
		ActualSize:       sg.ActualSize,
		Head:             api.LvolToProtoLvol(sg.Head),
		Snapshots:        snapshots,
	}
}

// instanceStateToProcessState maps the internal InstanceState to the proto
// ShardGroup.process_state string ("running"/"error"/"stopped").
func instanceStateToProcessState(s types.InstanceState) string {
	switch s {
	case types.InstanceStateRunning:
		return "running"
	case types.InstanceStateError:
		return "error"
	default:
		return "stopped"
	}
}

// refreshECSnapshotMapNoLock rebuilds the in-memory head/snapshot cache for
// ShardGroupGet from live lvol bdevs in this ShardGroup's lvstore. Caller must
// hold the shardgroup lock.
func (sg *ShardGroup) refreshECSnapshotMapNoLock(spdkClient *spdkclient.Client) error {
	filter := func(b *spdktypes.BdevInfo) bool {
		if b.DriverSpecific == nil || b.DriverSpecific.Lvol == nil {
			return false
		}
		return b.DriverSpecific.Lvol.LvolStoreUUID == sg.LvsUUID
	}

	bdevLvolMap, err := GetBdevLvolMapWithFilter(spdkClient, filter)
	if err != nil {
		return errors.Wrapf(err, "failed to get lvol map for shardgroup %s", sg.Name)
	}

	newSnapshotMap := map[string]*api.Lvol{}
	var newHead *api.Lvol

	for lvolName, bdev := range bdevLvolMap {
		if bdev.DriverSpecific == nil || bdev.DriverSpecific.Lvol == nil {
			continue
		}

		svcLvol := &api.Lvol{
			Name:              lvolName,
			SpecSize:          bdev.NumBlocks * uint64(bdev.BlockSize),
			ActualSize:        bdev.DriverSpecific.Lvol.NumAllocatedClusters * defaultClusterSize,
			Parent:            bdev.DriverSpecific.Lvol.BaseSnapshot,
			Children:          map[string]bool{},
			CreationTime:      bdev.CreationTime,
			UserCreated:       bdev.DriverSpecific.Lvol.Xattrs[spdkclient.UserCreated] == strconv.FormatBool(true),
			SnapshotTimestamp: bdev.DriverSpecific.Lvol.Xattrs[spdkclient.SnapshotTimestamp],
			SnapshotChecksum:  bdev.DriverSpecific.Lvol.Xattrs[spdkclient.SnapshotChecksum],
		}

		if lvolName == sg.HeadLvolName {
			newHead = svcLvol
			continue
		}

		if bdev.DriverSpecific.Lvol.Snapshot {
			newSnapshotMap[lvolName] = svcLvol
		}
	}

	if newHead == nil {
		return fmt.Errorf("failed to find head lvol %s in lvstore %s", sg.HeadLvolName, sg.LvsName)
	}

	for snapshotName, snapshotLvol := range newSnapshotMap {
		if snapshotLvol.Parent == "" {
			continue
		}
		if snapshotLvol.Parent == sg.HeadLvolName {
			continue
		}

		if parentSnapshot, ok := newSnapshotMap[snapshotLvol.Parent]; ok {
			parentSnapshot.Children[snapshotName] = true
		}
	}

	sg.Head = newHead
	sg.SnapshotMap = newSnapshotMap
	sg.ActualSize = newHead.ActualSize

	return nil
}

// Create materializes the EC stack on disk: NVMe-attach to all k+m shards,
// create bdev_ec, create lvstore on bdev_ec, create the head lvol, expose via
// NVMe-oF. The lvstore + head lvol persist across volume detach/re-attach;
// they are removed only by Delete with cleanupRequired=true.
func (sg *ShardGroup) Create(spdkClient *spdkclient.Client, superiorPortAllocator *commonbitmap.Bitmap) (ret *spdkrpc.ShardGroup, err error) {
	updateRequired := true

	sg.Lock()
	defer func() {
		sg.Unlock()
		if updateRequired {
			sg.UpdateCh <- nil
		}
	}()

	if sg.State == types.InstanceStateRunning {
		updateRequired = false
		return nil, grpcstatus.Errorf(grpccodes.AlreadyExists, "shardgroup %s already exists and running", sg.Name)
	}
	if sg.State != types.InstanceStatePending {
		updateRequired = false
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition, "invalid state %s for shardgroup %s creation", sg.State, sg.Name)
	}

	defer func() {
		if err != nil {
			sg.log.WithError(err).Errorf("Failed to create shardgroup %s", sg.Name)
			sg.State = types.InstanceStateError
			sg.ErrorMsg = err.Error()
			ret = ServiceShardGroupToProtoShardGroup(sg)
			err = nil
		} else {
			sg.ErrorMsg = ""
			sg.log.Info("Created shardgroup")
		}
	}()

	total := sg.DataChunks + sg.ParityChunks
	if uint32(len(sg.Shards)) != total {
		return nil, fmt.Errorf("shardgroup %s requires %d shards (dataChunks=%d + parityChunks=%d), got %d",
			sg.Name, total, sg.DataChunks, sg.ParityChunks, len(sg.Shards))
	}

	// Connect to all k+m shards and build base bdev list ordered by slot index.
	// In salvage mode, tolerate per-shard connection failures by passing "" at
	// the slot's position — the EC module marks that slot FAILED but continues
	// in degraded mode.
	baseBdevs := make([]string, total)
	for shardName, ep := range sg.Shards {
		if ep.SlotIndex >= total {
			return nil, fmt.Errorf("shard %s slot index %d exceeds dataChunks+parityChunks=%d", shardName, ep.SlotIndex, total)
		}
		if baseBdevs[ep.SlotIndex] != "" {
			return nil, fmt.Errorf("duplicate slot index %d: shard %s conflicts with an earlier shard", ep.SlotIndex, shardName)
		}
		controllerName := GetShardName(sg.VolumeName, ep.SlotIndex)
		bdevName, connErr := connectNVMfBdev(spdkClient, controllerName, ep.Address,
			ecShardCtrlrLossTimeoutSec, ecShardFastIOFailTimeoutSec, maxRetries, retryInterval)
		if connErr != nil {
			if sg.SalvageRequested {
				sg.log.WithError(connErr).Warnf("Salvage: failed to connect shard %s at %s; marking slot %d as missing", controllerName, ep.Address, ep.SlotIndex)
				baseBdevs[ep.SlotIndex] = ""
				continue
			}
			return nil, errors.Wrapf(connErr, "failed to connect shard %s at %s", controllerName, ep.Address)
		}
		baseBdevs[ep.SlotIndex] = bdevName
	}

	// Pre-check for a stale bdev_ec from a prior OFFLINE failure: SPDK's
	// bdev_ec OFFLINE cleanup closes descriptors to dead base bdevs but
	// leaves the EC bdev itself registered, so a recovery Create on the
	// same name otherwise fails with EEXIST. Delete the stale instance
	// first; the surviving shard lvols are untouched, and bdev_examine
	// on the new bdev_ec will rediscover the lvstore via
	// tryDiscoverExistingLvstore below.
	existing, err := spdkClient.BdevEcGetBdevs(sg.EcBdevName)
	if err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
		return nil, errors.Wrapf(err, "failed to check for existing EC bdev %s", sg.EcBdevName)
	}
	if len(existing) > 0 {
		sg.log.Warnf("Existing EC bdev %s found before create; deleting stale instance before recreating", sg.EcBdevName)
		if _, err := spdkClient.BdevEcDelete(sg.EcBdevName); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
			return nil, errors.Wrapf(err, "failed to delete stale EC bdev %s", sg.EcBdevName)
		}
	}

	// Create bdev_ec on the k+m base bdevs.
	sg.log.Infof("Creating EC bdev with dataChunks=%d parityChunks=%d stripSizeKb=%d baseBdevs=%v",
		sg.DataChunks, sg.ParityChunks, sg.StripSizeKb, baseBdevs)
	if _, err := spdkClient.BdevEcCreate(sg.EcBdevName, sg.DataChunks, sg.ParityChunks, sg.StripSizeKb, baseBdevs); err != nil {
		return nil, errors.Wrapf(err, "failed to create EC bdev %s", sg.EcBdevName)
	}

	// Always try to discover an existing lvstore/head first. This makes
	// clean detach -> recreate idempotent even when salvage_requested=false.
	// If nothing exists yet, fall back to fresh create.
	foundExistingLvstore, err := sg.tryDiscoverExistingLvstore(spdkClient)
	if err != nil {
		return nil, err
	}

	if !foundExistingLvstore {
		if sg.SalvageRequested {
			return nil, fmt.Errorf("salvage requested but existing lvstore/head not found for shardgroup %s", sg.Name)
		}

		// Fresh-create path: create lvstore + head lvol on bdev_ec.
		lvsUUID, err := spdkClient.BdevLvolCreateLvstore(sg.EcBdevName, sg.LvsName, 0)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create lvstore %s on EC bdev %s", sg.LvsName, sg.EcBdevName)
		}
		sg.LvsUUID = lvsUUID

		headLvolUUID, err := spdkClient.BdevLvolCreate("", lvsUUID, sg.HeadLvolName, util.BytesToMiB(sg.SpecSize), "", true)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create head lvol %s in lvstore %s", sg.HeadLvolName, sg.LvsName)
		}
		sg.HeadLvolUUID = headLvolUUID
	}

	// Expose the head lvol via NVMe-oF.
	if err := sg.prepareIPAndPort(superiorPortAllocator); err != nil {
		return nil, err
	}
	if err := spdkClient.StartExposeBdev(sg.Nqn, sg.HeadLvolUUID, generateNGUID(sg.HeadLvolName),
		sg.IP, strconv.Itoa(int(sg.Port))); err != nil {
		return nil, errors.Wrapf(err, "failed to expose head lvol for shardgroup %s", sg.Name)
	}
	sg.IsExposed = true

	if err := sg.refreshECSnapshotMapNoLock(spdkClient); err != nil {
		return nil, errors.Wrapf(err, "failed to refresh shardgroup snapshot/head cache for %s", sg.Name)
	}

	sg.State = types.InstanceStateRunning
	return ServiceShardGroupToProtoShardGroup(sg), nil
}

// Delete tears down the EC stack with cleanupRequired discipline.
//
// cleanupRequired=true (volume deletion path): full teardown — delete head
// lvol, delete lvstore, delete bdev_ec, disconnect NVMe-oF controllers.
//
// cleanupRequired=false (detach path): unexpose NVMe-oF + delete bdev_ec +
// disconnect NVMe-oF controllers, but **leave the lvstore and head lvol
// intact on the encoded shard blocks**. This is the central mechanism that
// prevents the EC-volume detach data-loss bug — re-attach reconstructs
// bdev_ec via SPDK's bdev_examine, which auto-imports the existing lvstore.
func (sg *ShardGroup) Delete(spdkClient *spdkclient.Client, cleanupRequired bool, superiorPortAllocator *commonbitmap.Bitmap) (err error) {
	updateRequired := false

	sg.Lock()
	defer func() {
		if err != nil {
			sg.log.WithError(err).Errorf("Failed to delete shardgroup with cleanupRequired=%v", cleanupRequired)
			if sg.State != types.InstanceStateError {
				sg.State = types.InstanceStateError
				sg.ErrorMsg = err.Error()
			}
		} else {
			sg.State = types.InstanceStateTerminating
			sg.ErrorMsg = ""
		}

		updateRequired = true

		sg.Unlock()

		if updateRequired {
			sg.UpdateCh <- nil
		}
	}()

	sg.log.Infof("Deleting shardgroup with cleanupRequired=%v", cleanupRequired)

	// 1. Stop NVMe-oF expose of the head lvol.
	if sg.IsExposed {
		if err := spdkClient.StopExposeBdev(sg.Nqn); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
			return errors.Wrapf(err, "failed to stop exposing shardgroup %s", sg.Name)
		}
		sg.IsExposed = false
	}

	// 2. Release allocated port.
	if sg.Port != 0 {
		if err := superiorPortAllocator.ReleaseRange(sg.Port, sg.Port); err != nil {
			return errors.Wrapf(err, "failed to release port %d during shardgroup %s deletion", sg.Port, sg.Name)
		}
		sg.Port = 0
	}

	// 3. Conditionally delete head lvol + lvstore (THE BUG FIX).
	//    cleanupRequired=false leaves the lvstore + head lvol on the encoded
	//    shard blocks so that re-attach can re-discover them via bdev_examine.
	if cleanupRequired {
		if sg.HeadLvolUUID != "" {
			if _, err := spdkClient.BdevLvolDelete(sg.HeadAlias); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
				return errors.Wrapf(err, "failed to delete head lvol %s for shardgroup %s", sg.HeadAlias, sg.Name)
			}
			sg.HeadLvolUUID = ""
		}
		if sg.LvsUUID != "" {
			if _, err := spdkClient.BdevLvolDeleteLvstore(sg.LvsName, ""); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
				return errors.Wrapf(err, "failed to delete lvstore %s for shardgroup %s", sg.LvsName, sg.Name)
			}
			sg.LvsUUID = ""
		}
	} else {
		sg.log.Info("Preserving lvstore + head lvol on bdev_ec for re-attach (cleanupRequired=false)")
	}

	// 4. Delete bdev_ec.
	if _, err := spdkClient.BdevEcDelete(sg.EcBdevName); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
		return errors.Wrapf(err, "failed to delete EC bdev %s for shardgroup %s", sg.EcBdevName, sg.Name)
	}

	// 5. Disconnect NVMe-oF controllers from all shards (best-effort).
	for shardName, ep := range sg.Shards {
		controllerName := GetShardName(sg.VolumeName, ep.SlotIndex)
		if _, detachErr := spdkClient.BdevNvmeDetachController(controllerName); detachErr != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(detachErr) {
			sg.log.WithError(detachErr).Warnf("Failed to detach NVMe controller %s for shard %s; continuing", controllerName, shardName)
		}
	}

	sg.log.Info("Deleted shardgroup")
	return nil
}

// Sync is a passive validator that mirrors Shard.Sync and Replica.Sync. It
// never re-allocates a port and never re-exposes the bdev. Behavior depends
// on the current state:
//
//   - Pending: walks to Stopped (newly discovered after process restart;
//     the ShardGroup controller's recovery path replaces or re-provisions).
//   - Running: validates that the live exposed port matches sg.Port; any
//     drift transitions to Error.
//   - other: no-op.
//
// In-place re-expose is deliberately avoided. Recovery of a ShardGroup
// process on a different node uses the salvage path (ShardGroupCreate with
// salvage_requested=true), not Sync.
func (sg *ShardGroup) Sync(spdkClient *spdkclient.Client) (err error) {
	sg.Lock()
	defer func() {
		if err != nil {
			sg.log.WithError(err).Errorf("Failed to sync shardgroup %s", sg.Name)
			sg.State = types.InstanceStateError
			sg.ErrorMsg = err.Error()
		}
		sg.Unlock()
		sg.UpdateCh <- nil
	}()

	if sg.State == types.InstanceStatePending {
		sg.State = types.InstanceStateStopped
		sg.ErrorMsg = ""
		sg.log.Debug("Synced shardgroup")
		return nil
	}

	if sg.State != types.InstanceStateRunning {
		return nil
	}

	subsystemMap, err := GetNvmfSubsystemMap(spdkClient)
	if err != nil {
		return err
	}

	exposedPort, exposedPortErr := getExposedPort(subsystemMap[sg.Nqn])
	if sg.IsExposed {
		if exposedPortErr != nil {
			return errors.Wrapf(exposedPortErr, "failed to find the actual port in subsystem NQN %s for shardgroup %s, which should be exposed at %d", sg.Nqn, sg.Name, sg.Port)
		}
		if exposedPort != sg.Port {
			return fmt.Errorf("found mismatching between the actual exposed port %d and the recorded port %d for exposed shardgroup %s", exposedPort, sg.Port, sg.Name)
		}
	} else if exposedPortErr == nil {
		return fmt.Errorf("found the actual port %d in subsystem NQN %s for shardgroup %s, which should not be exposed", exposedPort, sg.Nqn, sg.Name)
	}

	sg.ErrorMsg = ""
	sg.log.Debug("Synced shardgroup")
	return nil
}

// Expand grows the EC stack in place after each upstream shard has been
// resized: bdev_ec_resize → bdev_lvol_grow_lvstore → bdev_lvol_resize on the
// head lvol. The engine's raid1 layer auto-grows via NVMe AER when the
// exposed namespace size changes; no engine-side SPDK call is needed.
//
// All k+m shards must be resized via ShardExpand on their nodes before
// calling this.
func (sg *ShardGroup) Expand(spdkClient *spdkclient.Client, newSize uint64) (err error) {
	sg.Lock()
	defer sg.Unlock()

	roundedSize := util.RoundUp(newSize, helpertypes.MiB)
	if roundedSize != newSize {
		return fmt.Errorf("shardgroup %s: new size %d is not a multiple of MiB", sg.Name, newSize)
	}
	if sg.SpecSize > newSize {
		return fmt.Errorf("cannot shrink shardgroup %s from %d to %d", sg.Name, sg.SpecSize, newSize)
	}
	if sg.SpecSize == newSize {
		sg.log.Infof("Shardgroup %s already at size %d", sg.Name, newSize)
		return nil
	}

	sg.log.Infof("Expanding shardgroup to size %d", newSize)

	// 1. Resize bdev_ec to pick up the larger base bdevs.
	if _, err := spdkClient.BdevEcResize(sg.EcBdevName); err != nil {
		return errors.Wrapf(err, "failed to resize EC bdev %s", sg.EcBdevName)
	}

	// 2. Grow the lvstore to fill the resized bdev_ec.
	if _, err := spdkClient.BdevLvolGrowLvstore(sg.LvsName, ""); err != nil {
		return errors.Wrapf(err, "failed to grow lvstore %s", sg.LvsName)
	}

	// 3. Resize the head lvol.
	resized, err := spdkClient.BdevLvolResize(sg.HeadAlias, util.BytesToMiB(newSize))
	if err != nil {
		return errors.Wrapf(err, "failed to resize head lvol %s", sg.HeadAlias)
	}
	if !resized {
		return fmt.Errorf("head lvol %s was not resized", sg.HeadAlias)
	}

	if err := sg.refreshECSnapshotMapNoLock(spdkClient); err != nil {
		return errors.Wrapf(err, "failed to refresh shardgroup snapshot/head cache after expansion for %s", sg.Name)
	}

	sg.SpecSize = newSize
	sg.log.Info("Expanded shardgroup")
	return nil
}

// ExpandPrecheck validates that the ShardGroup's EC stack is in a state where
// expansion can proceed: no rebuild in progress, no scrub in progress, all
// slots NORMAL. Returns expansionRequired=true if the new size is larger than
// the current size and preconditions are met.
func (sg *ShardGroup) ExpandPrecheck(spdkClient *spdkclient.Client, newSize uint64) (expansionRequired bool, err error) {
	sg.RLock()
	defer sg.RUnlock()

	if sg.SpecSize >= newSize {
		return false, nil
	}

	bdevList, err := spdkClient.BdevEcGetBdevs(sg.EcBdevName)
	if err != nil {
		return false, errors.Wrapf(err, "failed to get EC bdev info for shardgroup %s", sg.Name)
	}
	if len(bdevList) != 1 {
		return false, fmt.Errorf("expected exactly one EC bdev for shardgroup %s, got %d", sg.Name, len(bdevList))
	}
	ecInfo := bdevList[0]

	if ecInfo.RebuildInProgress {
		return false, fmt.Errorf("shardgroup %s has a rebuild in progress", sg.Name)
	}
	if ecInfo.ReplaceInProgress {
		return false, fmt.Errorf("shardgroup %s has a slot replacement in progress", sg.Name)
	}
	if ecInfo.FailedCount > 0 {
		return false, fmt.Errorf("shardgroup %s has %d failed slot(s); cannot expand while degraded", sg.Name, ecInfo.FailedCount)
	}

	scrubProgress, err := spdkClient.BdevEcGetScrubProgress(sg.EcBdevName)
	if err != nil {
		return false, errors.Wrapf(err, "failed to query scrub progress for shardgroup %s", sg.Name)
	}
	if scrubProgress != nil {
		return false, fmt.Errorf("shardgroup %s has a scrub in progress", sg.Name)
	}

	return true, nil
}

// SnapshotCreate snapshots the head lvol under the given user-visible name.
// Returns the new snapshot's UUID.
func (sg *ShardGroup) SnapshotCreate(spdkClient *spdkclient.Client, snapshotName string) (snapshotUUID string, err error) {
	sg.Lock()
	defer sg.Unlock()

	if sg.State != types.InstanceStateRunning {
		return "", grpcstatus.Errorf(grpccodes.FailedPrecondition, "invalid state %s for shardgroup %s snapshot create", sg.State, sg.Name)
	}

	uuid, err := spdkClient.BdevLvolSnapshot(sg.HeadAlias, snapshotName, nil)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create snapshot %s on shardgroup %s", snapshotName, sg.Name)
	}

	if err := sg.refreshECSnapshotMapNoLock(spdkClient); err != nil {
		return "", errors.Wrapf(err, "failed to refresh shardgroup snapshot/head cache after snapshot create for %s", sg.Name)
	}

	sg.log.Infof("Created snapshot %s (uuid=%s)", snapshotName, uuid)
	return uuid, nil
}

// SnapshotDelete deletes the named snapshot from the lvstore. Idempotent for
// missing snapshots.
func (sg *ShardGroup) SnapshotDelete(spdkClient *spdkclient.Client, snapshotName string) error {
	sg.Lock()
	defer sg.Unlock()

	snapAlias := spdktypes.GetLvolAlias(sg.LvsName, snapshotName)
	if _, err := spdkClient.BdevLvolDelete(snapAlias); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
		return errors.Wrapf(err, "failed to delete snapshot %s from shardgroup %s", snapshotName, sg.Name)
	}

	if err := sg.refreshECSnapshotMapNoLock(spdkClient); err != nil {
		return errors.Wrapf(err, "failed to refresh shardgroup snapshot/head cache after snapshot delete for %s", sg.Name)
	}

	sg.log.Infof("Deleted snapshot %s", snapshotName)
	return nil
}

// SnapshotRevert replaces the head lvol with a fresh clone of the named
// snapshot. The caller (Volume controller) must ensure the engine has
// disconnected from this ShardGroup's NVMe-oF endpoint before invoking
// (FrontendEmpty guard at the engine layer).
//
// Sequence: unexpose head → delete head lvol → clone from snapshot →
// re-expose with the new head lvol UUID.
func (sg *ShardGroup) SnapshotRevert(spdkClient *spdkclient.Client, snapshotName string) error {
	sg.Lock()
	defer sg.Unlock()

	if sg.State != types.InstanceStateRunning {
		return grpcstatus.Errorf(grpccodes.FailedPrecondition, "invalid state %s for shardgroup %s snapshot revert", sg.State, sg.Name)
	}

	sg.log.Infof("Reverting shardgroup to snapshot %s", snapshotName)

	if sg.IsExposed {
		if err := spdkClient.StopExposeBdev(sg.Nqn); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
			return errors.Wrapf(err, "failed to stop exposing shardgroup %s before revert", sg.Name)
		}
		sg.IsExposed = false
	}

	if _, err := spdkClient.BdevLvolDelete(sg.HeadAlias); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
		return errors.Wrapf(err, "failed to delete head lvol %s during revert", sg.HeadAlias)
	}
	sg.HeadLvolUUID = ""

	snapAlias := spdktypes.GetLvolAlias(sg.LvsName, snapshotName)
	newHeadUUID, err := spdkClient.BdevLvolClone(snapAlias, sg.HeadLvolName)
	if err != nil {
		return errors.Wrapf(err, "failed to clone snapshot %s as new head", snapshotName)
	}
	sg.HeadLvolUUID = newHeadUUID

	if err := spdkClient.StartExposeBdev(sg.Nqn, sg.HeadLvolUUID, generateNGUID(sg.HeadLvolName),
		sg.IP, strconv.Itoa(int(sg.Port))); err != nil {
		return errors.Wrapf(err, "failed to re-expose head lvol after revert")
	}
	sg.IsExposed = true

	if err := sg.refreshECSnapshotMapNoLock(spdkClient); err != nil {
		return errors.Wrapf(err, "failed to refresh shardgroup snapshot/head cache after snapshot revert for %s", sg.Name)
	}

	sg.log.Infof("Reverted to snapshot %s (new head uuid=%s)", snapshotName, sg.HeadLvolUUID)
	return nil
}

// SnapshotPurge deletes orphan snapshots (snapshots with no clones) from this
// ShardGroup's lvstore. Snapshots that still have child clones are preserved.
// User-vs-system distinction is left to a future refinement.
func (sg *ShardGroup) SnapshotPurge(spdkClient *spdkclient.Client) error {
	sg.Lock()
	defer sg.Unlock()

	if sg.State != types.InstanceStateRunning {
		return grpcstatus.Errorf(grpccodes.FailedPrecondition, "invalid state %s for shardgroup %s snapshot purge", sg.State, sg.Name)
	}

	filter := func(b *spdktypes.BdevInfo) bool {
		if b.DriverSpecific == nil || b.DriverSpecific.Lvol == nil {
			return false
		}
		return b.DriverSpecific.Lvol.LvolStoreUUID == sg.LvsUUID
	}
	bdevLvolMap, err := GetBdevLvolMapWithFilter(spdkClient, filter)
	if err != nil {
		return errors.Wrapf(err, "failed to list lvols for shardgroup %s", sg.Name)
	}

	var purged int
	for lvolName, bdev := range bdevLvolMap {
		lvol := bdev.DriverSpecific.Lvol
		if !lvol.Snapshot || len(lvol.Clones) > 0 {
			continue
		}
		snapAlias := spdktypes.GetLvolAlias(sg.LvsName, lvolName)
		if _, err := spdkClient.BdevLvolDelete(snapAlias); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
			sg.log.WithError(err).Warnf("Failed to purge snapshot %s; continuing", lvolName)
			continue
		}
		sg.log.Infof("Purged orphan snapshot %s", lvolName)
		purged++
	}

	if err := sg.refreshECSnapshotMapNoLock(spdkClient); err != nil {
		return errors.Wrapf(err, "failed to refresh shardgroup snapshot/head cache after snapshot purge for %s", sg.Name)
	}

	sg.log.Infof("Snapshot purge complete: deleted %d orphan snapshot(s)", purged)
	return nil
}

// ShardReplace hot-swaps the bdev for a FAILED slot. It looks up the slot by
// shard name (the Shard CR external name <volumeName>-<slotIndex>),
// NVMe-connects to the new address, and calls bdev_ec_replace_base_bdev. The
// slot transitions FAILED → REPLACING and the new bdev immediately starts
// receiving foreground writes. ShardRebuildStart must follow to populate the
// pre-failure data.
func (sg *ShardGroup) ShardReplace(spdkClient *spdkclient.Client, shardName, shardAddress string) (slotState string, err error) {
	sg.Lock()
	defer sg.Unlock()

	if sg.State != types.InstanceStateRunning {
		return "", grpcstatus.Errorf(grpccodes.FailedPrecondition, "invalid state %s for shardgroup %s shard replace", sg.State, sg.Name)
	}

	ep, ok := sg.Shards[shardName]
	if !ok {
		return "", grpcstatus.Errorf(grpccodes.NotFound, "shard %s not found in shardgroup %s", shardName, sg.Name)
	}

	controllerName := GetShardName(sg.VolumeName, ep.SlotIndex)
	// Best-effort detach of any prior controller before re-attaching to the new address.
	if _, err := spdkClient.BdevNvmeDetachController(controllerName); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
		sg.log.WithError(err).Warnf("Pre-replace detach of controller %s failed; continuing", controllerName)
	}

	bdevName, err := connectNVMfBdev(spdkClient, controllerName, shardAddress,
		ecShardCtrlrLossTimeoutSec, ecShardFastIOFailTimeoutSec, maxRetries, retryInterval)
	if err != nil {
		return "", errors.Wrapf(err, "failed to connect replacement shard %s at %s", controllerName, shardAddress)
	}

	resp, err := spdkClient.BdevEcReplaceBaseBdev(sg.EcBdevName, ep.SlotIndex, bdevName)
	if err != nil {
		return "", errors.Wrapf(err, "failed to replace base bdev at slot %d for shardgroup %s", ep.SlotIndex, sg.Name)
	}

	// Record the new address; slot index is unchanged.
	ep.Address = shardAddress
	sg.Shards[shardName] = ep

	sg.log.Infof("Replaced shard %s at slot %d with %s; slot state=%s", shardName, ep.SlotIndex, shardAddress, resp.State)
	return string(resp.State), nil
}

// Bounds for the post-detach poll in ForceFailShard. The slot transition is
// driven by SPDK_BDEV_EVENT_REMOVE on the local bdev, which fires synchronously
// from BdevNvmeDetachController, so the wait is short in practice.
const (
	ecShardForceFailPollInterval = 100 * time.Millisecond
	ecShardForceFailPollTimeout  = 5 * time.Second
)

// ForceFailShard drives a slot to FAILED immediately by detaching the upstream
// NVMe controller, bypassing bdev_nvme's ctrlr_loss_timeout_sec wait. Used by
// the manager when it knows the shard is gone for good (intentional Shard CR
// delete, eviction) so that ShardGroupShardReplace can be issued in seconds
// instead of minutes.
//
// Idempotency rules — these defend against reconcile retries and stale
// requests racing past a successful replace:
//   - slot already FAILED: no-op, return current state.
//   - slot REPLACING: refuse with FailedPrecondition; a rebuild is in flight
//     and force-failing now would invalidate in-progress reconstruction.
//   - shardName unknown: NotFound.
//
// Failure accounting (failed_count, dirty-region bookkeeping, degraded-mode
// gating) is NOT touched here; it flows through the standard BDEV_EVENT_REMOVE
// path that bdev_ec already handles for unintentional failures. The only new
// behavior is the trigger.
func (sg *ShardGroup) ForceFailShard(spdkClient *spdkclient.Client, shardName string) (slotState string, err error) {
	sg.Lock()
	defer sg.Unlock()

	if sg.State != types.InstanceStateRunning {
		return "", grpcstatus.Errorf(grpccodes.FailedPrecondition, "invalid state %s for shardgroup %s shard force-fail", sg.State, sg.Name)
	}

	ep, ok := sg.Shards[shardName]
	if !ok {
		return "", grpcstatus.Errorf(grpccodes.NotFound, "shard %s not found in shardgroup %s", shardName, sg.Name)
	}

	currentState, err := sg.readSlotStateNoLock(spdkClient, ep.SlotIndex)
	if err != nil {
		return "", err
	}
	switch currentState {
	case spdktypes.BdevEcSlotStateFailed:
		return string(currentState), nil
	case spdktypes.BdevEcSlotStateReplacing:
		return "", grpcstatus.Errorf(grpccodes.FailedPrecondition,
			"shard %s in shardgroup %s is REPLACING; refuse to force-fail", shardName, sg.Name)
	}

	controllerName := GetShardName(sg.VolumeName, ep.SlotIndex)
	if _, err := spdkClient.BdevNvmeDetachController(controllerName); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
		return "", errors.Wrapf(err, "failed to detach controller %s for shard %s", controllerName, shardName)
	}

	deadline := time.Now().Add(ecShardForceFailPollTimeout)
	for {
		observed, err := sg.readSlotStateNoLock(spdkClient, ep.SlotIndex)
		if err != nil {
			return "", err
		}
		if observed == spdktypes.BdevEcSlotStateFailed {
			sg.log.Infof("Force-failed shard %s at slot %d", shardName, ep.SlotIndex)
			return string(observed), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("slot %d for shard %s did not transition to FAILED within %s (last state=%s)",
				ep.SlotIndex, shardName, ecShardForceFailPollTimeout, observed)
		}
		time.Sleep(ecShardForceFailPollInterval)
	}
}

// readSlotStateNoLock returns the current EC slot state for the given slot
// index. Caller must hold sg's lock (or an outer caller's equivalent).
func (sg *ShardGroup) readSlotStateNoLock(spdkClient *spdkclient.Client, slotIndex uint32) (spdktypes.BdevEcSlotState, error) {
	bdevList, err := spdkClient.BdevEcGetBdevs(sg.EcBdevName)
	if err != nil {
		return "", errors.Wrapf(err, "failed to get EC bdev info for shardgroup %s", sg.Name)
	}
	if len(bdevList) != 1 {
		return "", fmt.Errorf("expected exactly one EC bdev for shardgroup %s, got %d", sg.Name, len(bdevList))
	}
	for _, base := range bdevList[0].BaseBdevs {
		if base.Slot == slotIndex {
			return base.State, nil
		}
	}
	return "", fmt.Errorf("slot %d not found in EC bdev %s for shardgroup %s", slotIndex, sg.EcBdevName, sg.Name)
}

// ShardRebuildStart starts the background rebuild poller for all REPLACING
// slots. Returns the total stripe count and the first slot being rebuilt for
// observability.
func (sg *ShardGroup) ShardRebuildStart(spdkClient *spdkclient.Client) (numStripes uint64, firstSlot uint32, err error) {
	sg.Lock()
	defer sg.Unlock()

	if sg.State != types.InstanceStateRunning {
		return 0, 0, grpcstatus.Errorf(grpccodes.FailedPrecondition, "invalid state %s for shardgroup %s rebuild start", sg.State, sg.Name)
	}

	resp, err := spdkClient.BdevEcStartRebuild(sg.EcBdevName)
	if err != nil {
		return 0, 0, errors.Wrapf(err, "failed to start rebuild on shardgroup %s", sg.Name)
	}
	sg.log.Infof("Started rebuild for shardgroup %s: numStripes=%d firstSlot=%d", sg.Name, resp.NumStripes, resp.FirstSlot)
	return resp.NumStripes, resp.FirstSlot, nil
}

// ShardRebuildProgress queries the current rebuild progress.
func (sg *ShardGroup) ShardRebuildProgress(spdkClient *spdkclient.Client) (*spdktypes.BdevEcRebuildProgress, error) {
	sg.RLock()
	defer sg.RUnlock()

	progress, err := spdkClient.BdevEcGetRebuildProgress(sg.EcBdevName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get rebuild progress for shardgroup %s", sg.Name)
	}
	return &progress, nil
}

// ShardRebuildStop stops a running rebuild. Returns an error if no rebuild
// is in progress (-ENOENT from SPDK).
func (sg *ShardGroup) ShardRebuildStop(spdkClient *spdkclient.Client) error {
	sg.Lock()
	defer sg.Unlock()

	if _, err := spdkClient.BdevEcStopRebuild(sg.EcBdevName); err != nil {
		return errors.Wrapf(err, "failed to stop rebuild on shardgroup %s", sg.Name)
	}
	sg.log.Info("Stopped rebuild")
	return nil
}

// ShardRebuildQosSet sets the rebuild rate limit. maxStripesPerSec=0 means
// unlimited. paused=true suspends the rebuild poller without cancelling.
// Applied immediately to any in-progress rebuild.
func (sg *ShardGroup) ShardRebuildQosSet(spdkClient *spdkclient.Client, maxStripesPerSec uint32, paused bool) error {
	sg.Lock()
	defer sg.Unlock()

	if _, err := spdkClient.BdevEcSetRebuildQos(sg.EcBdevName, maxStripesPerSec, paused); err != nil {
		return errors.Wrapf(err, "failed to set rebuild QoS on shardgroup %s", sg.Name)
	}
	sg.log.Infof("Set rebuild QoS: maxStripesPerSec=%d paused=%v", maxStripesPerSec, paused)
	return nil
}

// tryDiscoverExistingLvstore queries SPDK for an already-imported per-volume
// lvstore/head pair after bdev_ec_create.
//
// Returns (true, nil) when both lvstore and head are found and cached on sg.
// Returns (false, nil) when no lvstore exists yet (fresh volume path).
// Returns error for malformed/mismatched existing state or RPC failures.
func (sg *ShardGroup) tryDiscoverExistingLvstore(spdkClient *spdkclient.Client) (bool, error) {
	lvstoreList, err := spdkClient.BdevLvolGetLvstore(sg.LvsName, "")
	if err != nil {
		if jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
			return false, nil
		}
		return false, errors.Wrapf(err, "failed to discover lvstore %s on EC bdev %s", sg.LvsName, sg.EcBdevName)
	}
	if len(lvstoreList) == 0 {
		return false, nil
	}
	if len(lvstoreList) != 1 {
		return false, fmt.Errorf("expected exactly one lvstore named %s, found %d", sg.LvsName, len(lvstoreList))
	}
	sg.LvsUUID = lvstoreList[0].UUID

	headBdev, err := spdkClient.BdevLvolGetByName(sg.HeadAlias, 0)
	if err != nil {
		if jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
			return false, fmt.Errorf("found lvstore %s but missing head lvol %s", sg.LvsName, sg.HeadAlias)
		}
		return false, errors.Wrapf(err, "failed to discover head lvol %s after auto-import", sg.HeadAlias)
	}
	sg.HeadLvolUUID = headBdev.UUID

	sg.log.Infof("Discovered existing lvstore %s (uuid=%s) and head lvol %s (uuid=%s)",
		sg.LvsName, sg.LvsUUID, sg.HeadLvolName, sg.HeadLvolUUID)
	return true, nil
}

// discoverExistingLvstore is the strict salvage-mode variant of
// tryDiscoverExistingLvstore: the lvstore/head pair must already exist.
func (sg *ShardGroup) discoverExistingLvstore(spdkClient *spdkclient.Client) error {
	found, err := sg.tryDiscoverExistingLvstore(spdkClient)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("salvage: expected existing lvstore/head for shardgroup %s, but none was found", sg.Name)
	}
	return nil
}

// prepareIPAndPort allocates a single port from the superior allocator and
// resolves the pod's storage-network IP. Both are recorded on the ShardGroup
// for the caller's NVMe-oF exposure.
func (sg *ShardGroup) prepareIPAndPort(superiorPortAllocator *commonbitmap.Bitmap) error {
	podIP, err := commonnet.GetIPForPod()
	if err != nil {
		return err
	}
	sg.IP = podIP

	port, _, err := superiorPortAllocator.AllocateRange(1)
	if err != nil {
		return err
	}
	sg.Port = port

	sg.log.Infof("Prepared IP %s and port %d for shardgroup", sg.IP, sg.Port)
	return nil
}
