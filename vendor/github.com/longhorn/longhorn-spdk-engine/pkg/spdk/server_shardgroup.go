package spdk

import (
	"context"

	logrus "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/emptypb"

	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/longhorn/types/pkg/generated/spdkrpc"

	"github.com/longhorn/longhorn-spdk-engine/pkg/types"
)

// ShardGroupCreate provisions the EC stack on this node: NVMe-attach to all
// k+m shards, create bdev_ec, create lvstore + head lvol, expose via NVMe-oF.
// With salvage_requested=true, dispatches to the recovery path that preserves
// the existing lvstore on bdev_ec (added in a follow-up commit).
func (s *Server) ShardGroupCreate(ctx context.Context, req *spdkrpc.ShardGroupCreateRequest) (ret *spdkrpc.ShardGroup, err error) {
	if req.Name == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shardgroup name is required")
	}
	if req.VolumeName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "volume name is required")
	}
	if req.SpecSize == 0 {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "spec_size is required")
	}
	if req.Spec == nil {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "spec is required")
	}
	if req.Spec.DataChunks == 0 || req.Spec.ParityChunks == 0 {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "data_chunks and parity_chunks are required")
	}

	sg, err := s.getOrCreateShardGroup(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		s.Lock()
		s.shardGroupMap[sg.Name] = sg
		s.Unlock()
	}()

	s.RLock()
	spdkClient := s.spdkClient
	s.RUnlock()

	return sg.Create(spdkClient, s.portAllocator)
}

// ShardGroupDelete tears down the ShardGroup process. cleanup_required=true
// removes the lvstore + head lvol; cleanup_required=false (the detach path)
// preserves them on the encoded shard blocks.
func (s *Server) ShardGroupDelete(ctx context.Context, req *spdkrpc.ShardGroupDeleteRequest) (ret *emptypb.Empty, err error) {
	if req.Name == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shardgroup name is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.Name]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return &emptypb.Empty{}, nil
	}

	if err := sg.Delete(spdkClient, req.CleanupRequired, s.portAllocator); err != nil {
		return nil, err
	}

	if req.CleanupRequired {
		s.Lock()
		delete(s.shardGroupMap, req.Name)
		s.Unlock()
	}

	return &emptypb.Empty{}, nil
}

func (s *Server) ShardGroupGet(ctx context.Context, req *spdkrpc.ShardGroupGetRequest) (ret *spdkrpc.ShardGroup, err error) {
	if req.Name == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shardgroup name is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.Name]
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.Name)
	}

	sg.RLock()
	defer sg.RUnlock()
	return ServiceShardGroupToProtoShardGroup(sg), nil
}

func (s *Server) ShardGroupList(ctx context.Context, req *emptypb.Empty) (*spdkrpc.ShardGroupListResponse, error) {
	res := map[string]*spdkrpc.ShardGroup{}

	s.RLock()
	sgMap := make(map[string]*ShardGroup, len(s.shardGroupMap))
	for k, v := range s.shardGroupMap {
		sgMap[k] = v
	}
	s.RUnlock()

	for name, sg := range sgMap {
		sg.RLock()
		res[name] = ServiceShardGroupToProtoShardGroup(sg)
		sg.RUnlock()
	}

	return &spdkrpc.ShardGroupListResponse{ShardGroups: res}, nil
}

// ShardGroupWatch streams ShardGroup state-change notifications.
func (s *Server) ShardGroupWatch(req *emptypb.Empty, srv spdkrpc.SPDKService_ShardGroupWatchServer) error {
	responseCh, err := s.Subscribe(types.InstanceTypeShardGroup)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			logrus.WithError(err).Error("SPDK service shardgroup watch errored out")
		} else {
			logrus.Info("SPDK service shardgroup watch ended successfully")
		}
	}()
	logrus.Info("Started new SPDK service shardgroup update watch")

	done := false
	for {
		select {
		case <-s.ctx.Done():
			logrus.Info("spdk gRPC server: stopped shardgroup watch due to the context done")
			done = true
		case <-responseCh:
			if err := srv.Send(&emptypb.Empty{}); err != nil {
				return err
			}
		}
		if done {
			break
		}
	}
	return nil
}

func (s *Server) ShardGroupExpand(ctx context.Context, req *spdkrpc.ShardGroupExpandRequest) (ret *emptypb.Empty, err error) {
	if req.Name == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shardgroup name is required")
	}
	if req.Size == 0 {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "size is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.Name]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.Name)
	}
	if err := sg.Expand(spdkClient, req.Size); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ShardGroupExpandPrecheck(ctx context.Context, req *spdkrpc.ShardGroupExpandPrecheckRequest) (*spdkrpc.ShardGroupExpandPrecheckResponse, error) {
	if req.Name == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shardgroup name is required")
	}
	if req.Size == 0 {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "size is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.Name]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.Name)
	}
	required, err := sg.ExpandPrecheck(spdkClient, req.Size)
	if err != nil {
		return nil, err
	}
	return &spdkrpc.ShardGroupExpandPrecheckResponse{ExpansionRequired: required}, nil
}

func (s *Server) ShardGroupShardReplace(ctx context.Context, req *spdkrpc.ShardGroupShardReplaceRequest) (*spdkrpc.ShardGroupShardReplaceResponse, error) {
	if req.ShardGroupName == "" || req.ShardName == "" || req.ShardAddress == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name, shard_name, and shard_address are required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	state, err := sg.ShardReplace(spdkClient, req.ShardName, req.ShardAddress)
	if err != nil {
		return nil, err
	}
	return &spdkrpc.ShardGroupShardReplaceResponse{SlotState: bdevEcSlotStateToProto(state)}, nil
}

func (s *Server) ShardGroupShardForceFail(ctx context.Context, req *spdkrpc.ShardGroupShardForceFailRequest) (*spdkrpc.ShardGroupShardForceFailResponse, error) {
	if req.ShardGroupName == "" || req.ShardName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name and shard_name are required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	state, err := sg.ForceFailShard(spdkClient, req.ShardName)
	if err != nil {
		return nil, err
	}
	return &spdkrpc.ShardGroupShardForceFailResponse{SlotState: bdevEcSlotStateToProto(state)}, nil
}

func (s *Server) ShardGroupShardRebuildStart(ctx context.Context, req *spdkrpc.ShardGroupShardRebuildStartRequest) (*spdkrpc.ShardGroupShardRebuildStartResponse, error) {
	if req.ShardGroupName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	numStripes, firstSlot, err := sg.ShardRebuildStart(spdkClient)
	if err != nil {
		return nil, err
	}
	return &spdkrpc.ShardGroupShardRebuildStartResponse{
		NumStripes: numStripes,
		FirstSlot:  firstSlot,
	}, nil
}

func (s *Server) ShardGroupShardRebuildProgress(ctx context.Context, req *spdkrpc.ShardGroupShardRebuildProgressRequest) (*spdkrpc.ShardGroupShardRebuildProgressResponse, error) {
	if req.ShardGroupName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	progress, err := sg.ShardRebuildProgress(spdkClient)
	if err != nil {
		return nil, err
	}
	return &spdkrpc.ShardGroupShardRebuildProgressResponse{
		CurrentSlot:     progress.CurrentSlot,
		CurrentStripe:   progress.CurrentStripe,
		NumStripes:      progress.NumStripes,
		StripesRebuilt:  progress.StripesRebuilt,
		SlotsToRebuild:  progress.SlotsToRebuild,
		PercentComplete: progress.PercentComplete,
	}, nil
}

func (s *Server) ShardGroupShardRebuildStop(ctx context.Context, req *spdkrpc.ShardGroupShardRebuildStopRequest) (*emptypb.Empty, error) {
	if req.ShardGroupName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	if err := sg.ShardRebuildStop(spdkClient); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ShardGroupShardRebuildQosSet(ctx context.Context, req *spdkrpc.ShardGroupShardRebuildQosSetRequest) (*emptypb.Empty, error) {
	if req.ShardGroupName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	if err := sg.ShardRebuildQosSet(spdkClient, req.MaxStripesPerSec, req.Paused); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ShardGroupSnapshotCreate(ctx context.Context, req *spdkrpc.ShardGroupSnapshotCreateRequest) (*spdkrpc.ShardGroupSnapshotCreateResponse, error) {
	if req.ShardGroupName == "" || req.SnapshotName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name and snapshot_name are required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	uuid, err := sg.SnapshotCreate(spdkClient, req.SnapshotName)
	if err != nil {
		return nil, err
	}
	return &spdkrpc.ShardGroupSnapshotCreateResponse{SnapshotUuid: uuid}, nil
}

func (s *Server) ShardGroupSnapshotDelete(ctx context.Context, req *spdkrpc.ShardGroupSnapshotDeleteRequest) (*emptypb.Empty, error) {
	if req.ShardGroupName == "" || req.SnapshotName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name and snapshot_name are required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	if err := sg.SnapshotDelete(spdkClient, req.SnapshotName); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ShardGroupSnapshotRevert(ctx context.Context, req *spdkrpc.ShardGroupSnapshotRevertRequest) (*emptypb.Empty, error) {
	if req.ShardGroupName == "" || req.SnapshotName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name and snapshot_name are required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	if err := sg.SnapshotRevert(spdkClient, req.SnapshotName); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ShardGroupSnapshotPurge(ctx context.Context, req *spdkrpc.ShardGroupSnapshotPurgeRequest) (*emptypb.Empty, error) {
	if req.ShardGroupName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard_group_name is required")
	}

	s.RLock()
	sg := s.shardGroupMap[req.ShardGroupName]
	spdkClient := s.spdkClient
	s.RUnlock()

	if sg == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shardgroup %v", req.ShardGroupName)
	}
	if err := sg.SnapshotPurge(spdkClient); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// bdevEcSlotStateToProto maps the BdevEcSlotState string ("normal"/"failed"/
// "replacing") to the spdkrpc.EcSlotState enum.
func bdevEcSlotStateToProto(state string) spdkrpc.EcSlotState {
	switch state {
	case "failed":
		return spdkrpc.EcSlotState_EC_SLOT_STATE_FAILED
	case "replacing":
		return spdkrpc.EcSlotState_EC_SLOT_STATE_REPLACING
	default:
		return spdkrpc.EcSlotState_EC_SLOT_STATE_NORMAL
	}
}

// getOrCreateShardGroup returns the existing in-memory ShardGroup keyed by
// req.Name, or constructs a fresh one in InstanceStatePending. Subsequent
// Create on the returned ShardGroup materializes the SPDK stack.
func (s *Server) getOrCreateShardGroup(req *spdkrpc.ShardGroupCreateRequest) (*ShardGroup, error) {
	s.Lock()
	defer s.Unlock()

	if sg, ok := s.shardGroupMap[req.Name]; ok {
		return sg, nil
	}

	shards := map[string]*ShardEndpoint{}
	for shardName, ep := range req.Spec.Shards {
		shards[shardName] = &ShardEndpoint{
			Address:   ep.Address,
			SlotIndex: ep.SlotIndex,
		}
	}

	return NewShardGroup(s.ctx, req.Name, req.VolumeName, req.SpecSize,
		req.Spec.DataChunks, req.Spec.ParityChunks, req.Spec.StripSizeKb,
		shards, req.Spec.SalvageRequested, s.updateChs[types.InstanceTypeShardGroup]), nil
}
