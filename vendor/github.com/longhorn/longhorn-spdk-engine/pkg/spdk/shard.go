package spdk

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"

	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/longhorn/go-spdk-helper/pkg/jsonrpc"
	"github.com/longhorn/types/pkg/generated/spdkrpc"

	commonbitmap "github.com/longhorn/go-common-libs/bitmap"
	commonnet "github.com/longhorn/go-common-libs/net"
	spdkclient "github.com/longhorn/go-spdk-helper/pkg/spdk/client"
	spdktypes "github.com/longhorn/go-spdk-helper/pkg/spdk/types"
	helpertypes "github.com/longhorn/go-spdk-helper/pkg/types"

	"github.com/longhorn/longhorn-spdk-engine/pkg/types"
	"github.com/longhorn/longhorn-spdk-engine/pkg/util"

	safelog "github.com/longhorn/longhorn-spdk-engine/pkg/log"
)

type Shard struct {
	sync.RWMutex

	ctx context.Context

	Name       string // shard-<volumeName>-<slotIndex>
	VolumeName string
	SlotIndex  uint32
	Alias      string // <lvsName>/<name>
	LvsName    string
	LvsUUID    string
	Nqn        string

	SizeBytes uint64
	UUID      string // lvol UUID populated after creation

	IP   string
	Port int32

	State    types.InstanceState
	ErrorMsg string

	IsExposed bool

	// UpdateCh should not be protected by the shard lock
	UpdateCh chan interface{}

	log *safelog.SafeLogger
}

func GetShardName(volumeName string, slotIndex uint32) string {
	return fmt.Sprintf("shard-%s-%d", volumeName, slotIndex)
}

func NewShard(ctx context.Context, volumeName string, slotIndex uint32, lvsName, lvsUUID string, sizeBytes uint64, updateCh chan interface{}) *Shard {
	name := GetShardName(volumeName, slotIndex)

	log := logrus.StandardLogger().WithFields(logrus.Fields{
		"shardName":  name,
		"volumeName": volumeName,
		"slotIndex":  slotIndex,
		"lvsName":    lvsName,
		"lvsUUID":    lvsUUID,
	})

	roundedSize := util.RoundUp(sizeBytes, helpertypes.MiB)
	if roundedSize != sizeBytes {
		log.Infof("Rounded up size from %v to %v since the size should be a multiple of MiB", sizeBytes, roundedSize)
	}
	log = log.WithField("sizeBytes", roundedSize)

	return &Shard{
		ctx: ctx,

		Name:       name,
		VolumeName: volumeName,
		SlotIndex:  slotIndex,
		Alias:      spdktypes.GetLvolAlias(lvsName, name),
		LvsName:    lvsName,
		LvsUUID:    lvsUUID,
		Nqn:        helpertypes.GetNQN(name),

		SizeBytes: roundedSize,

		State: types.InstanceStatePending,

		UpdateCh: updateCh,

		log: safelog.NewSafeLogger(log),
	}
}

func ServiceShardToProtoShard(s *Shard) *spdkrpc.Shard {
	state := spdkrpc.EcSlotState_EC_SLOT_STATE_NORMAL
	if s.State == types.InstanceStateError {
		state = spdkrpc.EcSlotState_EC_SLOT_STATE_FAILED
	}

	return &spdkrpc.Shard{
		ShardId:    s.Name,
		VolumeName: s.VolumeName,
		SlotIndex:  s.SlotIndex,
		State:      state,
		SizeBytes:  s.SizeBytes,
		LvsName:    s.LvsName,
		LvsUuid:    s.LvsUUID,
		BdevName:   s.Alias,
		NvmfNqn:    s.Nqn,
		Ip:         s.IP,
		Port:       s.Port,
		ErrorMsg:   s.ErrorMsg,
		Uuid:       s.UUID,
	}
}

func (s *Shard) Create(spdkClient *spdkclient.Client, superiorPortAllocator *commonbitmap.Bitmap) (ret *spdkrpc.Shard, err error) {
	updateRequired := true

	s.Lock()
	defer func() {
		s.Unlock()

		if updateRequired {
			s.UpdateCh <- nil
		}
	}()

	if s.State == types.InstanceStateRunning {
		updateRequired = false
		return nil, grpcstatus.Errorf(grpccodes.AlreadyExists, "shard %v already exists and running", s.Name)
	}
	if s.State != types.InstanceStatePending {
		updateRequired = false
		return nil, grpcstatus.Errorf(grpccodes.FailedPrecondition, "invalid state %s for shard %s creation", s.State, s.Name)
	}

	defer func() {
		if err != nil {
			s.log.WithError(err).Errorf("Failed to create shard %s", s.Name)
			s.State = types.InstanceStateError
			s.ErrorMsg = err.Error()
			ret = ServiceShardToProtoShard(s)
			err = nil
		} else {
			s.ErrorMsg = ""
			s.log.Info("Created shard")
		}
	}()

	if err := s.validateAndSyncLvstore(spdkClient); err != nil {
		return nil, err
	}

	if s.UUID == "" {
		uuid, err := spdkClient.BdevLvolCreate("", s.LvsUUID, s.Name, util.BytesToMiB(s.SizeBytes), "", true)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create lvol for shard %s", s.Name)
		}
		s.UUID = uuid
	}

	if err := s.prepareIPAndPort(superiorPortAllocator); err != nil {
		return nil, err
	}

	if err := spdkClient.StartExposeBdev(s.Nqn, s.UUID, generateNGUID(s.Name), s.IP, strconv.Itoa(int(s.Port))); err != nil {
		return nil, errors.Wrapf(err, "failed to expose bdev for shard %s", s.Name)
	}

	s.IsExposed = true
	s.State = types.InstanceStateRunning

	return ServiceShardToProtoShard(s), nil
}

func (s *Shard) Delete(spdkClient *spdkclient.Client, superiorPortAllocator *commonbitmap.Bitmap) (err error) {
	updateRequired := false

	s.Lock()
	defer func() {
		if err != nil {
			s.log.WithError(err).Errorf("Failed to delete shard")
			if s.State != types.InstanceStateError {
				s.State = types.InstanceStateError
				s.ErrorMsg = err.Error()
			}
		} else {
			s.State = types.InstanceStateTerminating
			s.ErrorMsg = ""
		}

		updateRequired = true

		s.Unlock()

		if updateRequired {
			s.UpdateCh <- nil
		}
	}()

	if s.IsExposed {
		s.log.Info("Stopping exposed bdev for shard deletion")
		if err := spdkClient.StopExposeBdev(s.Nqn); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
			return errors.Wrapf(err, "failed to stop exposing shard %s", s.Name)
		}
		s.IsExposed = false
	}

	if s.Port != 0 {
		if err := superiorPortAllocator.ReleaseRange(s.Port, s.Port); err != nil {
			return errors.Wrapf(err, "failed to release port %d during shard %s deletion", s.Port, s.Name)
		}
		s.Port = 0
	}

	if _, err := spdkClient.BdevLvolDelete(s.Alias); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
		return errors.Wrapf(err, "failed to delete lvol for shard %s", s.Name)
	}

	s.UUID = ""

	s.log.Info("Deleted shard")

	return nil
}

func (s *Shard) Sync(spdkClient *spdkclient.Client) (err error) {
	s.Lock()
	defer func() {
		if err != nil {
			s.log.WithError(err).Errorf("Failed to sync shard %s", s.Name)
			s.State = types.InstanceStateError
			s.ErrorMsg = err.Error()
		}
		s.Unlock()
		s.UpdateCh <- nil
	}()

	if s.State == types.InstanceStatePending {
		s.State = types.InstanceStateStopped
		s.ErrorMsg = ""
		s.log.Debug("Synced shard")
		return nil
	}

	if s.State != types.InstanceStateRunning {
		return nil
	}

	subsystemMap, err := GetNvmfSubsystemMap(spdkClient)
	if err != nil {
		return err
	}

	exposedPort, exposedPortErr := getExposedPort(subsystemMap[s.Nqn])
	if s.IsExposed {
		if exposedPortErr != nil {
			return errors.Wrapf(exposedPortErr, "failed to find the actual port in subsystem NQN %s for shard %s, which should be exposed at %d", s.Nqn, s.Name, s.Port)
		}
		if exposedPort != s.Port {
			return fmt.Errorf("found mismatching between the actual exposed port %d and the recorded port %d for exposed shard %s", exposedPort, s.Port, s.Name)
		}
	} else if exposedPortErr == nil {
		return fmt.Errorf("found the actual port %d in subsystem NQN %s for shard %s, which should not be exposed", exposedPort, s.Nqn, s.Name)
	}

	s.ErrorMsg = ""
	s.log.Debug("Synced shard")
	return nil
}

func (s *Shard) Get() *spdkrpc.Shard {
	s.RLock()
	defer s.RUnlock()

	return ServiceShardToProtoShard(s)
}

func (s *Shard) Expand(spdkClient *spdkclient.Client, newSize uint64) error {
	s.Lock()
	defer s.Unlock()

	s.log.Infof("Expanding shard to size %v", newSize)

	roundedSize := util.RoundUp(newSize, helpertypes.MiB)
	if roundedSize != newSize {
		return fmt.Errorf("shard %s rounded up size from %v to %v since the size should be a multiple of MiB", s.Name, newSize, roundedSize)
	}

	if s.SizeBytes > newSize {
		return fmt.Errorf("cannot shrink shard %s from %v to %v", s.Name, s.SizeBytes, newSize)
	}
	if s.SizeBytes == newSize {
		s.log.Infof("Shard %s already at size %v", s.Name, newSize)
		return nil
	}

	reExposeBdev := false
	if s.IsExposed {
		if err := spdkClient.StopExposeBdev(s.Nqn); err != nil && !jsonrpc.IsJSONRPCRespErrorNoSuchDevice(err) {
			return errors.Wrapf(err, "failed to stop exposing shard %s before expansion", s.Name)
		}
		s.IsExposed = false
		reExposeBdev = true
	}

	resized, err := spdkClient.BdevLvolResize(s.Alias, util.BytesToMiB(newSize))
	if !resized || err != nil {
		if err != nil {
			return errors.Wrapf(err, "failed to resize shard %s", s.Name)
		}
		return fmt.Errorf("shard %s was not resized", s.Name)
	}

	if reExposeBdev {
		if err := spdkClient.StartExposeBdev(s.Nqn, s.UUID, generateNGUID(s.Name), s.IP, strconv.Itoa(int(s.Port))); err != nil {
			return errors.Wrapf(err, "failed to re-expose shard %s after expansion", s.Name)
		}
		s.IsExposed = true
	}

	s.SizeBytes = newSize

	s.log.Info("Expanding shard complete")
	return nil
}

func (s *Shard) validateAndSyncLvstore(spdkClient *spdkclient.Client) error {
	var (
		lvsList []spdktypes.LvstoreInfo
		err     error
	)

	if s.LvsUUID != "" {
		lvsList, err = spdkClient.BdevLvolGetLvstore("", s.LvsUUID)
	} else if s.LvsName != "" {
		lvsList, err = spdkClient.BdevLvolGetLvstore(s.LvsName, "")
	}
	if err != nil {
		return err
	}
	if len(lvsList) != 1 {
		return fmt.Errorf("found zero or multiple lvstore with name %s and UUID %s during shard %s creation", s.LvsName, s.LvsUUID, s.Name)
	}
	if s.LvsName == "" {
		s.LvsName = lvsList[0].Name
	}
	if s.LvsUUID == "" {
		s.LvsUUID = lvsList[0].UUID
	}
	if s.LvsName != lvsList[0].Name || s.LvsUUID != lvsList[0].UUID {
		return fmt.Errorf("found mismatching between the actual lvstore name %s with UUID %s and the recorded lvstore name %s with UUID %s during shard %s creation", lvsList[0].Name, lvsList[0].UUID, s.LvsName, s.LvsUUID, s.Name)
	}

	return nil
}

func (s *Shard) prepareIPAndPort(superiorPortAllocator *commonbitmap.Bitmap) error {
	podIP, err := commonnet.GetIPForPod()
	if err != nil {
		return err
	}
	s.IP = podIP

	port, _, err := superiorPortAllocator.AllocateRange(1)
	if err != nil {
		return err
	}
	s.Port = port

	s.log.Infof("Prepared IP %s and port %d for shard", s.IP, s.Port)

	return nil
}
