package spdk

import (
	"context"

	"github.com/cockroachdb/errors"
	logrus "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/emptypb"

	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/longhorn/types/pkg/generated/spdkrpc"

	"github.com/longhorn/longhorn-spdk-engine/pkg/types"
)

func (s *Server) ShardCreate(ctx context.Context, req *spdkrpc.ShardCreateRequest) (ret *spdkrpc.Shard, err error) {
	if req.VolumeName == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "volume name is required")
	}
	if req.SizeBytes == 0 {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "size is required")
	}

	shard, err := s.getOrCreateShard(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		s.Lock()
		s.shardMap[shard.Name] = shard
		s.Unlock()
	}()

	s.RLock()
	spdkClient := s.spdkClient
	s.RUnlock()

	return shard.Create(spdkClient, s.portAllocator)
}

func (s *Server) ShardDelete(ctx context.Context, req *spdkrpc.ShardDeleteRequest) (ret *emptypb.Empty, err error) {
	if req.ShardId == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard ID is required")
	}

	s.RLock()
	shard := s.shardMap[req.ShardId]
	spdkClient := s.spdkClient
	s.RUnlock()

	if shard == nil {
		return &emptypb.Empty{}, nil
	}

	if err := shard.Delete(spdkClient, s.portAllocator); err != nil {
		return nil, err
	}

	s.Lock()
	delete(s.shardMap, req.ShardId)
	s.Unlock()

	return &emptypb.Empty{}, nil
}

func (s *Server) ShardGet(ctx context.Context, req *spdkrpc.ShardGetRequest) (ret *spdkrpc.Shard, err error) {
	if req.ShardId == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard ID is required")
	}

	s.RLock()
	shard := s.shardMap[req.ShardId]
	s.RUnlock()

	if shard == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shard %v", req.ShardId)
	}

	return shard.Get(), nil
}

func (s *Server) ShardList(ctx context.Context, req *emptypb.Empty) (*spdkrpc.ShardListResponse, error) {
	shardMap := map[string]*Shard{}
	res := map[string]*spdkrpc.Shard{}

	s.RLock()
	for k, v := range s.shardMap {
		shardMap[k] = v
	}
	s.RUnlock()

	for name, sh := range shardMap {
		res[name] = sh.Get()
	}

	return &spdkrpc.ShardListResponse{Shards: res}, nil
}

func (s *Server) ShardExpand(ctx context.Context, req *spdkrpc.ShardExpandRequest) (ret *emptypb.Empty, err error) {
	if req.ShardId == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "shard ID is required")
	}
	if req.NewSize == 0 {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "new size is required")
	}

	s.RLock()
	shard := s.shardMap[req.ShardId]
	spdkClient := s.spdkClient
	s.RUnlock()

	if shard == nil {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "cannot find shard %v", req.ShardId)
	}

	if err := shard.Expand(spdkClient, req.NewSize); err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

// ShardWatch returns a stream of shard updates
func (s *Server) ShardWatch(req *emptypb.Empty, srv spdkrpc.SPDKService_ShardWatchServer) error {
	responseCh, err := s.Subscribe(types.InstanceTypeShard)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			logrus.WithError(err).Error("SPDK service shard watch errored out")
		} else {
			logrus.Info("SPDK service shard watch ended successfully")
		}
	}()
	logrus.Info("Started new SPDK service shard update watch")

	done := false
	for {
		select {
		case <-s.ctx.Done():
			logrus.Info("spdk gRPC server: stopped shard watch due to the context done")
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

func (s *Server) getOrCreateShard(req *spdkrpc.ShardCreateRequest) (*Shard, error) {
	s.Lock()
	defer s.Unlock()

	name := GetShardName(req.VolumeName, req.SlotIndex)
	if shard, ok := s.shardMap[name]; ok {
		return shard, nil
	}

	if req.LvsName == "" && req.LvsUuid == "" {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "lvs_name or lvs_uuid is required for shard creation")
	}

	exists, err := s.isLvsExist(req.LvsUuid, req.LvsName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to check lvstore %v(%v) existence for shard %v creation", req.LvsName, req.LvsUuid, name)
	}
	if !exists {
		return nil, grpcstatus.Errorf(grpccodes.NotFound, "lvstore %v(%v) does not exist for shard %v creation", req.LvsName, req.LvsUuid, name)
	}

	return NewShard(s.ctx, req.VolumeName, req.SlotIndex, req.LvsName, req.LvsUuid, req.SizeBytes, s.updateChs[types.InstanceTypeShard]), nil
}
