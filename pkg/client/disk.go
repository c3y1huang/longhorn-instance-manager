package client

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	rpc "github.com/longhorn/types/pkg/generated/imrpc"

	"github.com/longhorn/longhorn-instance-manager/pkg/api"
	"github.com/longhorn/longhorn-instance-manager/pkg/meta"
	"github.com/longhorn/longhorn-instance-manager/pkg/types"
	"github.com/longhorn/longhorn-instance-manager/pkg/util"
)

type DiskServiceContext struct {
	cc *grpc.ClientConn

	ctx  context.Context
	quit context.CancelFunc

	service rpc.DiskServiceClient
	health  healthpb.HealthClient
}

func (c *DiskServiceClient) Close() error {
	if c.cc == nil {
		return nil
	}
	return c.cc.Close()
}

func (c *DiskServiceClient) getDiskServiceClient() rpc.DiskServiceClient {
	return c.service
}

type DiskServiceClient struct {
	serviceURL string
	tlsConfig  *tls.Config
	DiskServiceContext
}

// NewDiskServiceClient creates a new DiskServiceClient.
func NewDiskServiceClient(ctx context.Context, ctxCancel context.CancelFunc, serviceURL string, tlsConfig *tls.Config) (*DiskServiceClient, error) {
	getDiskServiceContext := func(serviceUrl string, tlsConfig *tls.Config) (DiskServiceContext, error) {
		connection, err := util.Connect(serviceUrl, tlsConfig)
		if err != nil {
			return DiskServiceContext{}, errors.Wrapf(err, "cannot connect to Disk Service %v", serviceUrl)
		}

		return DiskServiceContext{
			cc:      connection,
			ctx:     ctx,
			quit:    ctxCancel,
			service: rpc.NewDiskServiceClient(connection),
			health:  healthpb.NewHealthClient(connection),
		}, nil
	}

	serviceContext, err := getDiskServiceContext(serviceURL, tlsConfig)
	if err != nil {
		return nil, err
	}

	return &DiskServiceClient{
		serviceURL:         serviceURL,
		tlsConfig:          tlsConfig,
		DiskServiceContext: serviceContext,
	}, nil
}

// NewDiskServiceClientWithTLS creates a new DiskServiceClient with TLS
func NewDiskServiceClientWithTLS(ctx context.Context, ctxCancel context.CancelFunc, serviceURL, caFile, certFile, keyFile, peerName string) (*DiskServiceClient, error) {
	tlsConfig, err := util.LoadClientTLS(caFile, certFile, keyFile, peerName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load tls key pair from file")
	}

	return NewDiskServiceClient(ctx, ctxCancel, serviceURL, tlsConfig)
}

// DiskCreate creates a disk with the given name and path.
// diskUUID is optional, if not provided, it indicates the disk is newly added.
func (c *DiskServiceClient) DiskCreate(diskType, diskName, diskUUID, diskPath, diskDriver string, blockSize int64) (*api.DiskInfo, error) {
	if diskName == "" || diskPath == "" {
		return nil, fmt.Errorf("failed to create disk: missing required parameters")
	}

	t, ok := rpc.DiskType_value[diskType]
	if !ok {
		return nil, fmt.Errorf("failed to get disk info: invalid disk type %v", diskType)
	}

	client := c.getDiskServiceClient()
	ctx, cancel := context.WithTimeout(context.Background(), types.GRPCServiceTimeout)
	defer cancel()

	resp, err := client.DiskCreate(ctx, &rpc.DiskCreateRequest{
		DiskType:   rpc.DiskType(t),
		DiskName:   diskName,
		DiskUuid:   diskUUID,
		DiskPath:   diskPath,
		BlockSize:  blockSize,
		DiskDriver: diskDriver,
	})
	if err != nil {
		return nil, err
	}

	return &api.DiskInfo{
		ID:          resp.GetId(),
		Name:        resp.GetName(),
		UUID:        resp.GetUuid(),
		Path:        resp.GetPath(),
		Type:        resp.GetType(),
		Driver:      resp.GetDriver(),
		TotalSize:   resp.GetTotalSize(),
		FreeSize:    resp.GetFreeSize(),
		TotalBlocks: resp.GetTotalBlocks(),
		FreeBlocks:  resp.GetFreeBlocks(),
		BlockSize:   resp.GetBlockSize(),
		ClusterSize: resp.GetClusterSize(),
	}, nil
}

// DiskGet returns the disk info with the given name and path.
func (c *DiskServiceClient) DiskGet(diskType, diskName, diskPath, diskDriver string) (*api.DiskInfo, error) {
	if diskName == "" {
		return nil, fmt.Errorf("failed to get disk info: missing required parameter diskName")
	}

	t, ok := rpc.DiskType_value[diskType]
	if !ok {
		return nil, fmt.Errorf("failed to get disk info: invalid disk type %v", diskType)
	}

	client := c.getDiskServiceClient()
	ctx, cancel := context.WithTimeout(context.Background(), types.GRPCServiceTimeout)
	defer cancel()

	resp, err := client.DiskGet(ctx, &rpc.DiskGetRequest{
		DiskType:   rpc.DiskType(t),
		DiskName:   diskName,
		DiskPath:   diskPath,
		DiskDriver: diskDriver,
	})
	if err != nil {
		return nil, err
	}

	return &api.DiskInfo{
		ID:          resp.GetId(),
		Name:        resp.GetName(),
		UUID:        resp.GetUuid(),
		Path:        resp.GetPath(),
		Type:        resp.GetType(),
		Driver:      resp.GetDriver(),
		TotalSize:   resp.GetTotalSize(),
		FreeSize:    resp.GetFreeSize(),
		TotalBlocks: resp.GetTotalBlocks(),
		FreeBlocks:  resp.GetFreeBlocks(),
		BlockSize:   resp.GetBlockSize(),
		ClusterSize: resp.GetClusterSize(),
	}, nil
}

// DiskDelete deletes the disk with the given name, disk name, disk UUID, disk path and disk driver.
func (c *DiskServiceClient) DiskDelete(diskType, diskName, diskUUID, diskPath, diskDriver string) error {
	if diskName == "" {
		return fmt.Errorf("failed to delete disk: missing required diskName")
	}

	client := c.getDiskServiceClient()
	ctx, cancel := context.WithTimeout(context.Background(), types.GRPCServiceTimeout)
	defer cancel()

	_, err := client.DiskDelete(ctx, &rpc.DiskDeleteRequest{
		DiskType:   rpc.DiskType(rpc.DiskType_value[diskType]),
		DiskName:   diskName,
		DiskUuid:   diskUUID,
		DiskPath:   diskPath,
		DiskDriver: diskDriver,
	})
	return err
}

func (c *DiskServiceClient) DiskReplicaInstanceList(diskType, diskName, diskDriver string) (map[string]*api.ReplicaStorageInstance, error) {
	if diskName == "" {
		return nil, fmt.Errorf("failed to list replica instances on disk: missing required parameter")
	}

	client := c.getDiskServiceClient()
	ctx, cancel := context.WithTimeout(context.Background(), types.GRPCServiceTimeout)
	defer cancel()

	resp, err := client.DiskReplicaInstanceList(ctx, &rpc.DiskReplicaInstanceListRequest{
		DiskType:   rpc.DiskType(rpc.DiskType_value[diskType]),
		DiskName:   diskName,
		DiskDriver: diskDriver,
	})
	if err != nil {
		return nil, err
	}

	instances := map[string]*api.ReplicaStorageInstance{}
	for name, instance := range resp.ReplicaInstances {
		instances[name] = &api.ReplicaStorageInstance{
			Name:       instance.Name,
			UUID:       instance.Uuid,
			DiskName:   instance.DiskName,
			DiskUUID:   instance.DiskUuid,
			SpecSize:   instance.SpecSize,
			ActualSize: instance.ActualSize,
		}
	}

	return instances, nil
}

// DiskReplicaInstanceDelete deletes the replica instance with the given name on the disk.
func (c *DiskServiceClient) DiskReplicaInstanceDelete(diskType, diskName, diskUUID, diskDriver, replciaInstanceName string) error {
	if diskName == "" || diskUUID == "" || replciaInstanceName == "" {
		return fmt.Errorf("failed to delete replica instance on disk: missing required parameters")
	}

	client := c.getDiskServiceClient()
	ctx, cancel := context.WithTimeout(context.Background(), types.GRPCServiceTimeout)
	defer cancel()

	_, err := client.DiskReplicaInstanceDelete(ctx, &rpc.DiskReplicaInstanceDeleteRequest{
		DiskType:            rpc.DiskType(rpc.DiskType_value[diskType]),
		DiskName:            diskName,
		DiskUuid:            diskUUID,
		DiskDriver:          diskDriver,
		ReplciaInstanceName: replciaInstanceName,
	})

	return err
}

// VersionGet returns the disk service version.
func (c *DiskServiceClient) VersionGet() (*meta.DiskServiceVersionOutput, error) {
	client := c.getDiskServiceClient()
	ctx, cancel := context.WithTimeout(context.Background(), types.GRPCServiceTimeout)
	defer cancel()

	resp, err := client.VersionGet(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get disk service version")
	}

	return &meta.DiskServiceVersionOutput{
		Version:   resp.Version,
		GitCommit: resp.GitCommit,
		BuildDate: resp.BuildDate,

		InstanceManagerDiskServiceAPIVersion:    int(resp.InstanceManagerDiskServiceAPIVersion),
		InstanceManagerDiskServiceAPIMinVersion: int(resp.InstanceManagerDiskServiceAPIMinVersion),
	}, nil
}

func (c *DiskServiceClient) CheckConnection() error {
	req := &healthpb.HealthCheckRequest{}
	_, err := c.health.Check(getContextWithGRPCTimeout(c.ctx), req)
	return err
}

// MetricsGet returns the disk metrics with the given name and path.
func (c *DiskServiceClient) MetricsGet(diskType, diskName, diskPath, diskDriver string) (*api.DiskMetrics, error) {
	if diskName == "" {
		return nil, fmt.Errorf("failed to get disk metrics: missing required parameter diskName")
	}

	t, ok := rpc.DiskType_value[diskType]
	if !ok {
		return nil, fmt.Errorf("failed to get disk metrics: invalid disk type %v", diskType)
	}

	client := c.getDiskServiceClient()
	ctx, cancel := context.WithTimeout(context.Background(), types.GRPCServiceTimeout)
	defer cancel()

	resp, err := client.MetricsGet(ctx, &rpc.DiskGetRequest{
		DiskType:   rpc.DiskType(t),
		DiskName:   diskName,
		DiskPath:   diskPath,
		DiskDriver: diskDriver,
	})
	if err != nil {
		return nil, err
	}

	// Convert to api.DiskMetrics format
	return &api.DiskMetrics{
		ReadThroughput:  resp.Metrics.ReadThroughput,
		WriteThroughput: resp.Metrics.WriteThroughput,
		ReadLatency:     resp.Metrics.ReadLatency,
		WriteLatency:    resp.Metrics.WriteLatency,
		ReadIOPS:        resp.Metrics.ReadIOPS,
		WriteIOPS:       resp.Metrics.WriteIOPS,
	}, nil
}
